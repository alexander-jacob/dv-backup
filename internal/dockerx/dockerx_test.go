package dockerx

import "testing"

func TestContainerInUse(t *testing.T) {
	for state, want := range map[string]bool{
		StateRunning:    true,
		StatePaused:     true,
		StateRestarting: true,
		StateExited:     false,
		StateCreated:    false,
		"dead":          false,
		"removing":      false,
	} {
		if got := (Container{State: state}).InUse(); got != want {
			t.Errorf("InUse() for state %q = %v, want %v", state, got, want)
		}
	}
}
