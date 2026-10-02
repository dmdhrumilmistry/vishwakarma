#!/bin/sh
# Waits for Android to boot, sizes a virtual display to the device screen,
# mirrors the device into it with scrcpy and serves it over VNC. Restarts
# scrcpy if it exits (device reboot, adb drop).
set -u

TARGET="${ADB_TARGET:-127.0.0.1:5555}"
PORT="${VNC_PORT:-5900}"
MAX="${SCREEN_MAX:-1280}"

log() { echo "android-screen: $*"; }

wait_boot() {
	while :; do
		adb connect "$TARGET" >/dev/null 2>&1
		if [ "$(adb -s "$TARGET" shell getprop sys.boot_completed 2>/dev/null | tr -d '\r')" = "1" ]; then
			return
		fi
		sleep 2
	done
}

log "waiting for Android at $TARGET"
wait_boot

# "Physical size: 720x1280" (an override line may follow; the last wins).
size=$(adb -s "$TARGET" shell wm size 2>/dev/null | tr -d '\r' | sed -n 's/.*: *\([0-9]*x[0-9]*\).*/\1/p' | tail -1)
w=${size%x*}
h=${size#*x}
case "$w$h" in
	*[!0-9]* | "") w=720 h=1280 ;;
esac
# Scale so the long side is at most $MAX, keeping even dimensions.
if [ "$h" -gt "$MAX" ] || [ "$w" -gt "$MAX" ]; then
	if [ "$h" -ge "$w" ]; then
		w=$((w * MAX / h / 2 * 2)) h=$MAX
	else
		h=$((h * MAX / w / 2 * 2)) w=$MAX
	fi
fi
log "device screen ${size:-unknown}, display ${w}x${h}"

# Xvnc is the X display and the VNC server. Only the Vishwakarma server can
# reach this port (NetworkPolicy), and it authenticates users itself.
# The clipboard syncs both ways: VNC <-> X CLIPBOARD <-> scrcpy <-> Android.
Xtigervnc :0 -geometry "${w}x${h}" -depth 24 -rfbport "$PORT" -SecurityTypes None \
	-AlwaysShared -AcceptCutText -SendCutText -SetPrimary=0 -SendPrimary=0 \
	-nolisten tcp &
sleep 1

# Clipboard bridge (browser -> device). Text pasted in the browser arrives
# through Xvnc as the X clipboard; scrcpy does not pick up external X
# clipboard owners in this headless setup, so type the text into the focused
# field with adb instead (ASCII; Enter for newlines). After typing, take the
# clipboard over under scrcpy's MIME type: that marks the text as consumed,
# and a new paste (even of the same text) makes Xvnc the owner again.
# Device -> browser needs no help: scrcpy copies the device clipboard to X
# and Xvnc sends it to the browser.
MIME='text/plain;charset=utf-8'
type_text() {
	n=0
	printf '%s\n' "$1" | while IFS= read -r line; do
		[ "$n" -gt 0 ] && adb -s "$TARGET" shell input keyevent 66
		n=$((n + 1))
		[ -z "$line" ] && continue
		# Quote for the device shell; input text reads %s as a space.
		esc=$(printf '%s' "$line" | sed -e "s/'/'\"'\"'/g" -e 's/ /%s/g')
		adb -s "$TARGET" shell "input text '$esc'" >/dev/null 2>&1
	done
}
(
	while :; do
		targets=$(timeout 2 xclip -o -selection clipboard -t TARGETS </dev/null 2>/dev/null)
		case "$targets" in
			*"$MIME"* | "") ;;
			*UTF8_STRING*)
				text=$(timeout 2 xclip -o -selection clipboard -t UTF8_STRING </dev/null 2>/dev/null)
				printf '%s' "$text" | xclip -i -selection clipboard -t "$MIME" >/dev/null 2>&1
				if [ -n "$text" ]; then
					log "typing pasted text (${#text} characters)"
					type_text "$text"
				fi
				;;
		esac
		sleep 0.2
	done
) &

while :; do
	wait_boot
	log "mirroring"
	scrcpy -s "$TARGET" --no-audio --window-borderless --window-x 0 --window-y 0 \
		--window-width "$w" --window-height "$h" --max-size "$MAX" --max-fps 30 \
		--video-bit-rate 4M --stay-awake --window-title android
	log "scrcpy exited ($?), restarting"
	sleep 2
done
