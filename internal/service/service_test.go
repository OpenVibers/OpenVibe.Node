package service

import (
	"context"
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
}
