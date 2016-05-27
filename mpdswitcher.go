package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type backendStatus int

const (
	stateDown backendStatus = iota
	statePaused
	statePlaying
)

type backend struct {
	addr     string
	password string
	state    backendStatus
}

var (
	port         = flag.Int("port", 6600, "Port to run on")
	backendsFlag = flag.String("backends", "", "Backends to support")
	backendsFile = flag.String("backends_file", "", "File with more backends to support")

	backends []backend

	availableBackends int
	mtx               sync.Mutex
	unavailable       = sync.NewCond(&mtx)
	sock              net.Listener
	recentToggles     int32
	preferredBackend  int
	trafficStats      int64
)

func main() {
	flag.Parse()
	b := strings.Split(*backendsFlag, ",")
	if *backendsFile != "" {
		d, err := ioutil.ReadFile(*backendsFile)
		if err != nil {
			log.Fatal(err)
		}
		lines := strings.Split(string(d), "\n")
		b = append(b, lines...)
	}
	for _, addr := range b {
		if addr == "" {
			continue
		}
		be := backend{
			addr: addr,
		}
		ex := strings.Split(addr, "@")
		if len(ex) == 2 {
			be.addr = ex[1]
			be.password = ex[0]
		}
		backends = append(backends, be)
	}

	if len(backends) == 0 {
		log.Fatal("No backends specified")
	}

	for id := range backends {
		go handleBackend(id)
	}

	for {
		mtx.Lock()
		for availableBackends == 0 {
			unavailable.Wait()
		}

		var err error
		sock, err = net.Listen("tcp", fmt.Sprintf(":%d", *port))
		if err != nil {
			log.Fatalf("Listen failed: %v", err)
		}
		mtx.Unlock()

		for {
			conn, err := sock.Accept()
			if err != nil {
				if strings.Contains(err.Error(), "use of closed network connection") {
					break
				}
				log.Printf("Accept failed: %s\n", err)
			}
			log.Printf("[%s] connected", conn.RemoteAddr())
			go func(conn net.Conn) {
				defer conn.Close()
				err := handleConnection(conn)
				if err != nil {
					log.Printf("[%s] Closed: %v", conn.RemoteAddr(), err)
				}
			}(conn)
		}
		sock.Close()
	}
}

func setAvailability(id int, state backendStatus) {
	mtx.Lock()
	defer mtx.Unlock()
	fmt.Printf("State[%s #%d]: %d\n", backends[id].addr, id, state)
	if state != backends[id].state {
		if state == stateDown {
			availableBackends--
			if availableBackends == 0 {
				sock.Close()
			}
		} else if backends[id].state == stateDown {
			availableBackends++
			if availableBackends == 1 {
				unavailable.Broadcast()
			}
		}
	}
	backends[id].state = state
}

func handleBackend(id int) {
	addr := backends[id].addr
	for {
		state, err := getBackendStatus(id)
		fmt.Printf("State[%s]: %d (%v)\n", addr, state, err)
		if err != nil {
			log.Printf("[%s] Failed: %s", addr, err)
		}
		setAvailability(id, state)
		time.Sleep(time.Minute)
	}
}

func getBackendStatus(id int) (backendStatus, error) {
	conn, err := net.Dial("tcp", backends[id].addr)
	if err != nil {
		return stateDown, err
	}
	defer conn.Close()
	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		return stateDown, scanner.Err()
	}
	line := scanner.Text()
	if !strings.HasPrefix(line, "OK MPD ") {
		return stateDown, fmt.Errorf("Got a weird greeting: %s", line)
	}
	if backends[id].password != "" {
		fmt.Fprintf(conn, "password %s\n", backends[id].password)
		if !scanner.Scan() {
			return stateDown, scanner.Err()
		}
	}
	io.WriteString(conn, "status\n")
	state := statePaused
	for scanner.Scan() {
		line := scanner.Text()
		if line == "state: play" {
			state = statePlaying
		}
		if line == "OK" {
			break
		}
		if strings.HasPrefix(line, "ACK") {
			return state, fmt.Errorf("unexpected error: %s", line)
		}
	}
	return state, scanner.Err()
}

func handleConnection(conn net.Conn) error {
	choice := preferredBackend
	var mpd net.Conn
	for {
		mtx.Lock()
	Loop:
		for id, be := range backends {
			switch be.state {
			case statePlaying:
				choice = id
				break Loop
			case statePaused:
				if backends[choice].state == stateDown {
					choice = id
				}
			}
		}
		mtx.Unlock()
		if backends[choice].state == stateDown {
			return errors.New("No backend available")
		}
		var err error
		mpd, err = net.Dial("tcp", backends[choice].addr)
		if err != nil {
			setAvailability(choice, stateDown)
			return err
		}
		defer mpd.Close()
		break
	}
	done := make(chan error, 1)
	inStatus := false
	go func() {
		scanner := bufio.NewScanner(io.TeeReader(mpd, conn))
		for scanner.Scan() {
			line := scanner.Text()
			atomic.AddInt64(&trafficStats, int64(len(line)+1))
			// fmt.Println(line)
			if line == "OK" || strings.HasPrefix(line, "ACK") {
				inStatus = false
			}
			if inStatus && strings.HasPrefix(line, "state: ") {
				if line == "state: play" {
					setAvailability(choice, statePlaying)
				} else {
					setAvailability(choice, statePaused)
				}
			}
		}
		done <- scanner.Err()
	}()
	go func() {
		scanner := bufio.NewScanner(io.TeeReader(conn, mpd))
		for scanner.Scan() {
			line := scanner.Text()
			atomic.AddInt64(&trafficStats, int64(len(line)+1))
			fmt.Println(line)
			if line == "status" {
				inStatus = true
			}
			if line == "mpdswitcher_stats" {
				fmt.Fprintf(conn, "traffic_bytes: %d\n", atomic.LoadInt64(&trafficStats))
			}
			if line == "play" || strings.HasPrefix(line, "playid ") || strings.HasPrefix(line, "pause") {
				mtx.Lock()
				wantPlaying := backends[choice].state != statePlaying
				mtx.Unlock()
				if (line == "play" || strings.HasPrefix(line, "playid")) != (strings.HasPrefix(line, "pause") && strings.Contains(line, "1")) {
					wantPlaying = false
					setAvailability(choice, statePaused)
				} else if strings.HasPrefix(line, "pause") && strings.Contains(line, "0") {
					wantPlaying = true
					mtx.Lock()
					preferredBackend = choice
					mtx.Unlock()
					setAvailability(choice, statePlaying)
				}

				v := atomic.AddInt32(&recentToggles, 1)
				go func() {
					time.Sleep(time.Second)
					atomic.AddInt32(&recentToggles, -1)
				}()
				if v >= 4 && v <= 5 && !wantPlaying {
					mtx.Lock()
					preferredBackend = (choice + 1) % len(backends)
					fmt.Printf("New preferred backend: %d\n", preferredBackend)
					mtx.Unlock()
					break
				}
			}
		}
		done <- scanner.Err()
	}()
	return <-done
}
