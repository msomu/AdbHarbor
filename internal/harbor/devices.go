package harbor

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

type Device struct {
	Serial      string
	State       string
	Model       string
	TransportID string
	USB         bool
}

// ListDevices runs `adb devices -l` against the real adb and parses it.
func ListDevices(realADB string) ([]Device, error) {
	if realADB == "" {
		return nil, fmt.Errorf("real adb path not configured; run `adbharbor install`")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, realADB, "devices", "-l").Output()
	if err != nil {
		return nil, fmt.Errorf("adb devices: %w", err)
	}
	var devs []Device
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "List of devices") || strings.HasPrefix(line, "*") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		d := Device{Serial: fields[0], State: fields[1]}
		for _, f := range fields[2:] {
			switch {
			case strings.HasPrefix(f, "model:"):
				d.Model = strings.TrimPrefix(f, "model:")
			case strings.HasPrefix(f, "transport_id:"):
				d.TransportID = strings.TrimPrefix(f, "transport_id:")
			case strings.HasPrefix(f, "usb:"):
				d.USB = true
			}
		}
		devs = append(devs, d)
	}
	return devs, nil
}
