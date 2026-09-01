//go:build integration

package cli

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/reyoung/pika-go/internal/maintenance"
)

type integrationResumeCheckpointFlags struct {
	fd    *int
	point *string
}

func addIntegrationResumeCheckpointFlags(flags *flag.FlagSet) integrationResumeCheckpointFlags {
	return integrationResumeCheckpointFlags{
		fd:    flags.Int("maintenance-resume-checkpoint-fd", -1, "integration checkpoint file descriptor"),
		point: flags.String("maintenance-resume-checkpoint", "", "integration checkpoint name"),
	}
}

func (flags integrationResumeCheckpointFlags) decorate(ctx context.Context) (context.Context, func(), error) {
	if *flags.fd < 0 && *flags.point == "" {
		directory := os.Getenv("PIKA_GO_INTEGRATION_RESUME_CHECKPOINT_DIR")
		if directory == "" {
			return ctx, func() {}, nil
		}
		if !filepath.IsAbs(directory) {
			return ctx, func() {}, errors.New("integration checkpoint directory must be absolute")
		}
		checkpoint := func(checkpointCtx context.Context, reached string) error {
			enabled := filepath.Join(directory, reached+".enable")
			if _, err := os.Stat(enabled); errors.Is(err, os.ErrNotExist) {
				return nil
			} else if err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(directory, reached+".reached"), []byte(reached+"\n"), 0o600); err != nil {
				return err
			}
			continued := filepath.Join(directory, reached+".continue")
			for {
				if _, err := os.Stat(continued); err == nil {
					return nil
				} else if !errors.Is(err, os.ErrNotExist) {
					return err
				}
				select {
				case <-checkpointCtx.Done():
					return checkpointCtx.Err()
				case <-time.After(10 * time.Millisecond):
				}
			}
		}
		uninstall := maintenance.InstallIntegrationResumeCheckpoint(checkpoint)
		return ctx, uninstall, nil
	}
	if *flags.fd < 0 || (*flags.point != "intent_persisted" && *flags.point != "runtime_started") {
		return ctx, func() {}, errors.New("checkpoint fd and a supported checkpoint name are required")
	}
	checkpointFile := os.NewFile(uintptr(*flags.fd), "maintenance-resume-checkpoint")
	if checkpointFile == nil {
		return ctx, func() {}, errors.New("checkpoint file descriptor is invalid")
	}
	checkpointReader := bufio.NewReader(checkpointFile)
	checkpoint := func(checkpointCtx context.Context, reached string) error {
		if reached != *flags.point {
			return nil
		}
		if _, err := fmt.Fprintf(checkpointFile, "%s\n", reached); err != nil {
			return err
		}
		response := make(chan error, 1)
		go func() {
			line, err := checkpointReader.ReadString('\n')
			if err == nil && strings.TrimSpace(line) != "continue "+reached {
				err = fmt.Errorf("unexpected checkpoint response %q", strings.TrimSpace(line))
			}
			response <- err
		}()
		select {
		case err := <-response:
			return err
		case <-checkpointCtx.Done():
			return checkpointCtx.Err()
		}
	}
	uninstall := maintenance.InstallIntegrationResumeCheckpoint(checkpoint)
	return ctx, func() {
		uninstall()
		_ = checkpointFile.Close()
	}, nil
}
