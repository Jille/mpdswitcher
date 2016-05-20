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

var (
	port         = flag.Int("port", 6600, "Port to run on")
	backends     = flag.String("backends", "", "Backends to support")
	backendsFile = flag.String("backends_file", "", "File with more backends to support")

	backendAddrs     []string
	backendPasswords []string
	backendStates    []backendStatus

	availableBackends int
	mtx               sync.Mutex
	unavailable       = sync.NewCond(&mtx)
	sock              net.Listener
	recentToggles     int32
	preferredBackend  int
)

func main() {
	flag.Parse()
	b := strings.Split(*backends, ",")
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
		ex := strings.Split(addr, "@")
		if len(ex) == 2 {
			backendAddrs = append(backendAddrs, ex[1])
			backendPasswords = append(backendPasswords, ex[0])
		} else {
			backendPasswords = append(backendPasswords, "")
			backendAddrs = append(backendAddrs, ex[0])
		}
		backendStates = append(backendStates, stateDown)
	}

	if len(backendAddrs) == 0 {
		log.Fatal("No backends specified")
	}

	for id := range backendAddrs {
		go handleBackend(id)
	}

	for {
		mtx.Lock()
		for availableBackends == 0 {
			unavailable.Wait()
		}
		mtx.Unlock()

		var err error
		sock, err = net.Listen("tcp", fmt.Sprintf(":%d", *port))
		if err != nil {
			log.Fatalf("Listen failed: %v", err)
		}

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
	fmt.Printf("State[%s #%d]: %d\n", backendAddrs[id], id, state)
	if state != backendStates[id] {
		if state == stateDown {
			availableBackends--
			if availableBackends == 0 {
				sock.Close()
			}
		} else {
			availableBackends++
			if availableBackends == 1 {
				unavailable.Broadcast()
			}
		}
	}
	backendStates[id] = state
}

func handleBackend(id int) {
	addr := backendAddrs[id]
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
	conn, err := net.Dial("tcp", backendAddrs[id])
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
	if backendPasswords[id] != "" {
		fmt.Fprintf(conn, "password %s\n", backendPasswords[id])
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
		for id, state := range backendStates {
			switch state {
			case statePlaying:
				choice = id
				break Loop
			case statePaused:
				if backendStates[choice] == stateDown {
					choice = id
				}
			}
		}
		mtx.Unlock()
		if backendStates[choice] == stateDown {
			return errors.New("No backend available")
		}
		var err error
		mpd, err = net.Dial("tcp", backendAddrs[choice])
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
			fmt.Println(line)
			if line == "status" {
				inStatus = true
			}
			if strings.HasPrefix(line, "play") || strings.HasPrefix(line, "pause") {
				mtx.Lock()
				wantPlaying := backendStates[choice] != statePlaying
				mtx.Unlock()
				if strings.HasPrefix(line, "play") != strings.Contains(line, "1") {
					wantPlaying = false
					setAvailability(choice, statePaused)
				} else if strings.Contains(line, "0") {
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
					preferredBackend = (choice + 1) % len(backendAddrs)
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