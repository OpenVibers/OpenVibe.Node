package service

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestProgramStartStop(t *testing.T) {
	stopped := make(chan struct{})
	p := &program{run: func(ctx context.Context) error {
		<-ctx.Done()
		time.Sleep(10 * time.Millisecond) // stopping actuators
		close(stopped)
		return nil
	}}
	if err := p.Start(nil); err != nil {
		t.Fatal(err)
	}
	if err := p.Stop(nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stopped:
	default:
		t.Fatal("Stop returned before the program finished")
	}
}

func TestConfig(t *testing.T) {
	c := config(Options{Arguments: []string{"run"}})
	if c.Name != "openvibe-node" || c.Arguments[0] != "run" || c.Option["Restart"] != "always" {
		t.Fatalf("%+v", c)
	}
	if u, _ := c.Option["SystemdScript"].(string); !strings.Contains(u, "\n[Service]\n") || !strings.Contains(u, "\nDelegate=yes\n") {
		t.Fatalf("the systemd unit must delegate the Node's cgroup: %q", u)
	}
}
