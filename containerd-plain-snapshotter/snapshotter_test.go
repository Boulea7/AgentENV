package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/snapshots"
)

func TestRegisterRootfsRejectsInvalidConfig(t *testing.T) {
	for _, tc := range []struct {
		name string
		data string
	}{
		{name: "malformed", data: "{"},
		{name: "array", data: "[]"},
		{name: "null", data: "null"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			s, err := NewSnapshotter(root)
			if err != nil {
				t.Fatal(err)
			}
			rootfs := t.TempDir()
			configPath := filepath.Join(root, "config.json")
			data := []byte(tc.data)
			if err := os.WriteFile(configPath, data, 0600); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				got, err := os.ReadFile(configPath)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(got, data) {
					t.Errorf("invalid config was changed: got %q, want %q", got, data)
				}
			})
			defer func() {
				if p := recover(); p != nil {
					t.Errorf("RegisterRootfs panicked: %v", p)
				}
			}()

			err = s.RegisterRootfs("sha256:"+strings.Repeat("a", 64), rootfs)
			if err == nil {
				t.Fatal("RegisterRootfs accepted invalid config")
			}
			if !strings.Contains(err.Error(), configPath) {
				t.Errorf("error %q does not include config path %q", err, configPath)
			}
		})
	}
}

func TestConfigReadersReportErrors(t *testing.T) {
	for _, tc := range []struct {
		name      string
		data      string
		directory bool
	}{
		{name: "malformed", data: "{"},
		{name: "array", data: "[]"},
		{name: "null", data: "null"},
		{name: "directory", directory: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, method := range []string{"NewSnapshotter", "Stat", "Mounts", "Prepare", "View", "Walk"} {
				t.Run(method, func(t *testing.T) {
					ctx := context.Background()
					root := t.TempDir()
					s, err := NewSnapshotter(root)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := s.Prepare(ctx, "existing", ""); err != nil {
						t.Fatal(err)
					}
					configPath := filepath.Join(root, "config.json")
					if tc.directory {
						if err := os.Mkdir(configPath, 0700); err != nil {
							t.Fatal(err)
						}
					} else if err := os.WriteFile(configPath, []byte(tc.data), 0600); err != nil {
						t.Fatal(err)
					}
					before := directoryNames(t, root)
					key := "sha256:" + strings.Repeat("b", 64)
					walkCalls := 0
					switch method {
					case "NewSnapshotter":
						_, err = NewSnapshotter(root)
					case "Stat":
						_, err = s.Stat(ctx, key)
					case "Mounts":
						_, err = s.Mounts(ctx, key)
					case "Prepare":
						_, err = s.Prepare(ctx, "new", key)
					case "View":
						_, err = s.View(ctx, "new", key)
					case "Walk":
						err = s.Walk(ctx, func(context.Context, snapshots.Info) error {
							walkCalls++
							return nil
						})
					}
					if err == nil {
						t.Error("expected a config read error")
					} else {
						if !strings.Contains(err.Error(), configPath) {
							t.Errorf("error %q does not include config path %q", err, configPath)
						}
						if errors.Is(err, errdefs.ErrNotFound) {
							t.Errorf("config read error reported as snapshot not found: %v", err)
						}
						if tc.directory {
							var pathErr *os.PathError
							if !errors.As(err, &pathErr) || pathErr.Op != "read" || pathErr.Path != configPath {
								t.Errorf("error does not wrap the config read failure: %v", err)
							}
						}
					}
					if walkCalls != 0 {
						t.Errorf("Walk called its callback %d times after a config read failure", walkCalls)
					}
					if got := directoryNames(t, root); got != before {
						t.Errorf("config read failure changed root entries: got %q, want %q", got, before)
					}
					if tc.directory {
						info, err := os.Stat(configPath)
						if err != nil || !info.IsDir() {
							t.Fatalf("config directory was changed: %v", err)
						}
						if err := os.Remove(configPath); err != nil {
							t.Fatal(err)
						}
					} else {
						got, err := os.ReadFile(configPath)
						if err != nil {
							t.Fatal(err)
						}
						if string(got) != tc.data {
							t.Errorf("invalid config was changed: got %q, want %q", got, tc.data)
						}
					}
					if method == "Prepare" || method == "View" {
						if err := os.WriteFile(configPath, []byte("{}"), 0600); err != nil {
							t.Fatal(err)
						}
						if _, err := s.Stat(ctx, "new"); !errors.Is(err, errdefs.ErrNotFound) {
							t.Errorf("failed operation created a snapshot: Stat returned %v", err)
						}
					}
				})
			}
		})
	}
}

func TestRegisterRootfsPreservesMappingsAndRecoversAfterRepair(t *testing.T) {
	root := t.TempDir()
	s, err := NewSnapshotter(root)
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "config.json")
	if _, err := os.Stat(configPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fresh snapshotter unexpectedly created a config: %v", err)
	}
	mappings := map[string]string{
		"sha256:" + strings.Repeat("a", 64): t.TempDir(),
		"sha256:" + strings.Repeat("b", 64): t.TempDir(),
	}
	for key, rootfs := range mappings {
		if err := s.RegisterRootfs(key, rootfs); err != nil {
			t.Fatal(err)
		}
	}
	valid, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var stored map[string]string
	if err := json.Unmarshal(valid, &stored); err != nil {
		t.Fatal(err)
	}
	if len(stored) != len(mappings) {
		t.Fatalf("registered mappings were lost: got %v, want %v", stored, mappings)
	}
	for key, rootfs := range mappings {
		if stored[key] != rootfs {
			t.Fatalf("mapping %q = %q, want %q", key, stored[key], rootfs)
		}
	}
	if err := os.WriteFile(configPath, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	key := "sha256:" + strings.Repeat("c", 64)
	rootfs := t.TempDir()
	if err := s.RegisterRootfs(key, rootfs); err == nil {
		t.Fatal("registration accepted a damaged config")
	}
	// Restore the saved valid file, then retry on the same snapshotter.
	if err := os.WriteFile(configPath, valid, 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterRootfs(key, rootfs); err != nil {
		t.Fatalf("registration failed after repairing config: %v", err)
	}
	mappings[key] = rootfs
	reloaded, err := NewSnapshotter(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for key, rootfs := range mappings {
		for _, lookup := range []string{key, "moby/2/" + key} {
			info, err := reloaded.Stat(ctx, lookup)
			if err != nil || info.Name != lookup || info.Kind != snapshots.KindCommitted {
				t.Errorf("Stat(%q) = %+v, %v", lookup, info, err)
			}
			mounts, err := reloaded.Mounts(ctx, lookup)
			if err != nil || len(mounts) != 1 || mounts[0].Source != rootfs {
				t.Errorf("Mounts(%q) = %+v, %v; want source %q", lookup, mounts, err, rootfs)
			}
		}
	}
	walked := make(map[string]bool)
	if err := reloaded.Walk(ctx, func(_ context.Context, info snapshots.Info) error {
		walked[info.Name] = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(walked) != len(mappings) {
		t.Errorf("Walk returned %v, want %v", walked, mappings)
	}
	for key := range mappings {
		if !walked[key] {
			t.Errorf("Walk missed %q", key)
		}
	}
	if mounts, err := reloaded.Prepare(ctx, "active", key); err != nil || len(mounts) != 1 || mounts[0].Source != rootfs {
		t.Errorf("Prepare from configured parent = %+v, %v; want source %q", mounts, err, rootfs)
	}
	if mounts, err := reloaded.View(ctx, "view", "moby/2/"+key); err != nil || len(mounts) != 1 || mounts[0].Source != rootfs {
		t.Errorf("View from namespaced configured parent = %+v, %v; want source %q", mounts, err, rootfs)
	}
}

func TestConfigErrorsDoNotAffectInMemoryLookups(t *testing.T) {
	root := t.TempDir()
	s, err := NewSnapshotter(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	mounts, err := s.Prepare(ctx, "active", "")
	if err != nil || len(mounts) != 1 {
		t.Fatalf("Prepare = %+v, %v", mounts, err)
	}
	if err := os.WriteFile(filepath.Join(root, "config.json"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if info, err := s.Stat(ctx, "active"); err != nil || info.Name != "active" || info.Kind != snapshots.KindActive {
		t.Errorf("in-memory Stat = %+v, %v", info, err)
	}
	if got, err := s.Mounts(ctx, "active"); err != nil || len(got) != 1 || got[0].Source != mounts[0].Source {
		t.Errorf("in-memory Mounts = %+v, %v; want source %q", got, err, mounts[0].Source)
	}
}

func directoryNames(t *testing.T, path string) string {
	t.Helper()
	entries, err := os.ReadDir(path)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return strings.Join(names, ",")
}
