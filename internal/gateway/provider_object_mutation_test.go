package gateway

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/vault"
)

func TestProviderObjectPublicationJoinsMutationAndReleasesBeforeReplication(t *testing.T) {
	key, err := vault.New(bytes.Repeat([]byte{23}, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer key.Close()
	s := &Service{resourceObjectDir: t.TempDir(), resourceObjectSealer: key}
	s.resourceObjectReplicator = func(ctx context.Context, name string, raw []byte) error {
		return s.WithProviderObjectMutation(ctx, func() error { return nil })
	}
	entered, resume := make(chan struct{}), make(chan struct{})
	retirement := make(chan error, 1)
	go func() {
		retirement <- s.WithProviderObjectMutation(context.Background(), func() error { close(entered); <-resume; return nil })
	}()
	<-entered
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	written := make(chan error, 1)
	go func() {
		_, err := s.writeResourceObjectContext(ctx, "resource", "project", objectRoleContent, []byte("private"))
		written <- err
	}()
	select {
	case err := <-written:
		close(resume)
		<-retirement
		t.Fatal("publication crossed retirement", err)
	case <-time.After(20 * time.Millisecond):
	}
	cancel()
	close(resume)
	if err := <-retirement; err != nil {
		t.Fatal(err)
	}
	if err := <-written; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.resourceObjectDir, "resource.content")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("canceled writer published", err)
	}
	if entries, err := os.ReadDir(s.resourceObjectDir); err != nil || len(entries) != 0 {
		t.Fatal("canceled original temporary not retired", entries, err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := s.writeResourceObjectContext(context.Background(), "resource", "project", objectRoleContent, []byte("private"))
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("replication callback inherited object mutation gate")
	}
	if raw, err := os.ReadFile(filepath.Join(s.resourceObjectDir, "resource.content")); err != nil || !vault.SealedEnvelope(raw) || bytes.Contains(raw, []byte("private")) {
		t.Fatal("published content is not sealed", err)
	}
}
