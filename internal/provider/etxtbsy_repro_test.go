package provider

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"testing"
)

func TestETXTBSYExecutablePublicationStress(t *testing.T) {
	if os.Getenv("PIKA_GO_ETXTBSY_STRESS") != "1" {
		t.Skip("set PIKA_GO_ETXTBSY_STRESS=1 to run the executable publication stress harness")
	}
	iterations := 2000
	if raw := os.Getenv("PIKA_GO_ETXTBSY_ITERATIONS"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value <= 0 {
			t.Fatalf("invalid PIKA_GO_ETXTBSY_ITERATIONS %q", raw)
		}
		iterations = value
	}
	root := t.TempDir()
	script := []byte("#!/bin/sh\nexit 0\n")
	type failure struct {
		path string
		err  error
	}
	failures := make(chan failure, iterations*2)
	var wait sync.WaitGroup
	for index := 0; index < iterations; index++ {
		index := index
		for _, fixture := range []struct {
			name  string
			write func(string) error
		}{
			{name: "atomic-rename", write: func(path string) error {
				return writeProviderFile(path, script, 0o700)
			}},
			{name: "fresh-write-file", write: func(path string) error {
				return os.WriteFile(path, script, 0o700)
			}},
		} {
			fixture := fixture
			wait.Add(1)
			go func() {
				defer wait.Done()
				directory := filepath.Join(root, fmt.Sprintf("%s-%d", fixture.name, index))
				if err := os.Mkdir(directory, 0o700); err != nil {
					failures <- failure{path: fixture.name, err: err}
					return
				}
				path := filepath.Join(directory, "cursor-agent")
				if err := fixture.write(path); err != nil {
					failures <- failure{path: fixture.name, err: err}
					return
				}
				if err := exec.Command(path).Run(); err != nil {
					failures <- failure{path: fixture.name, err: err}
				}
			}()
		}
	}
	wait.Wait()
	close(failures)
	counts := map[string]int{}
	var other []failure
	for failure := range failures {
		if errors.Is(failure.err, syscall.ETXTBSY) {
			counts[failure.path]++
		} else {
			other = append(other, failure)
		}
	}
	if len(other) != 0 {
		t.Fatalf("non-ETXTBSY publication failures: %+v", other[:1])
	}
	if counts["atomic-rename"] != 0 || counts["fresh-write-file"] != 0 {
		t.Fatalf("ETXTBSY counts: atomic-rename=%d fresh-write-file=%d",
			counts["atomic-rename"], counts["fresh-write-file"])
	}
}
