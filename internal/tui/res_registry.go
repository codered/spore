package tui

import (
	"fmt"
	"time"
)

// resources is every view in hotkey order. The header's hint grid, the :
// command completion and the hotkeys all come from it.
func resources() []Resource {
	return []Resource{skillsRes{}, agentsRes{}, usageRes{}, jobsRes{}}
}

func resourceByName(name string) (Resource, bool) {
	for _, r := range resources() {
		if r.Name() == name {
			return r, true
		}
	}
	return nil, false
}

func resourceByHotkey(key string) (Resource, bool) {
	for _, r := range resources() {
		if r.Hotkey() == key {
			return r, true
		}
	}
	return nil, false
}

// age renders a duration the way k9s does: 45s, 12m, 3h, 2d.
func age(d time.Duration) string {
	if d < 0 {
		d = -d
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}
