# mpdswitcher
A proxy for MPD which switches between multiple MPD instances.

If you run multiple MPD servers, you can point MPD switcher at them and point
your clients at MPD switcher. MPD switcher will pick the MPD playing which is
currently playing.

To switch between servers manually you can play/pause 5 times within a second.
You'll be disconnected and when you connect again you'll be connected with the
next available servers.

## Usage
```shell
mpdswitcher --backends=password@somehost:6600,otherpassword@otherhost:6601
```

or
```shell
cat <<EOF > backends.txt
password@somehost:6600
otherpassword@otherhost:6601
EOF
mpdswitcher --backends_file=backends.txt
```
