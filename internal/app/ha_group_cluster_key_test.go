package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/replication"
)

func haGroupKeyCandidateFixture(t *testing.T) (config.Config, replication.MemberState, []byte, []byte, [32]byte) {
	t.Helper()
	cfg := testConfig(t)
	cfg.Storage.DataDir = t.TempDir()
	cfg.Replication = &config.Replication{ClusterID: "group", NodeID: "member1", Peers: []config.ReplicationPeer{{Name: "member2", Address: "127.0.0.1:9982", SPKISHA256: "sha256:" + strings.Repeat("1", 64)}}}
	state := replication.MemberState{Version: replication.StateVersion, ClusterID: "group", Incarnation: "inc_group", NodeID: "member1", Role: replication.RoleReplica, Term: 9, PromisedTerm: 11, DurableIndex: 7, ConfirmedIndex: 6, AppliedIndex: 5, OrderingHeadMAC: sha256.Sum256([]byte("original ordering")), Projection: replication.ProjectionState{Index: 5, MetadataEpoch: 2, MetadataSequence: 3}, Peers: []replication.StatePeer{{Name: "member2", Address: cfg.Replication.Peers[0].Address, SPKISHA256: cfg.Replication.Peers[0].SPKISHA256}}}
	old, next := bytes.Repeat([]byte{21}, 32), bytes.Repeat([]byte{23}, 32)
	key, err := replication.DeriveClusterKey(old, state.Incarnation)
	if err != nil {
		t.Fatal(err)
	}
	if err := replication.WriteState(cfg.ReplicationStatePath(), state, key[:]); err != nil {
		t.Fatal(err)
	}
	return cfg, state, old, next, key
}

func TestHAGroupClusterKeyCandidateActualReaderCloseFailureRetainsOwner(t *testing.T) {
	cfg, _, _, next, key := haGroupKeyCandidateFixture(t)
	raw, err := encodeHAGroupClusterKeyCandidate(cfg, key, next)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(haGroupClusterKeyPath(cfg), raw, 0600); err != nil {
		t.Fatal(err)
	}
	unknown := errors.New("actual reader Close completion is unknown")
	for _, failParent := range []bool{false, true} {
		calls := make(map[string]int)
		actualKey, incarnation, found, err := readHAGroupClusterKeyCandidateWithClose(cfg, next, func(file *os.File) error {
			calls[file.Name()]++
			closed := file.Close()
			if (file.Name() == cfg.Storage.DataDir) == failParent {
				return errors.Join(closed, unknown)
			}
			return closed
		})
		var debt *haGroupClusterKeyReadFailure
		if !found || actualKey != ([32]byte{}) || incarnation != ([32]byte{}) || !errors.Is(err, ownedfile.ErrRetirement) || !errors.Is(err, unknown) || !errors.As(err, &debt) {
			t.Fatal("uncertain actual reader retirement leaked candidate or dropped owner", err)
		}
		retained := 0
		for _, file := range debt.files {
			if file != nil {
				retained++
			}
		}
		if retained != 1 || calls[cfg.Storage.DataDir] != 1 || calls[haGroupClusterKeyPath(cfg)] != 1 {
			t.Fatal("unknown Close retried or other actual reader failed to join", retained, calls)
		}
		if !bytes.Equal(debt.raw[:], raw) {
			t.Fatal("retained actual read bytes disappeared before retirement")
		}
	}
}

func TestHAGroupClusterKeySingleMemberRotationRefusesBeforeAnySourceWrite(t *testing.T) {
	cfg, _, old, _, _ := haGroupKeyCandidateFixture(t)
	before, err := os.ReadFile(cfg.ReplicationStatePath())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RotateMasterKey(context.Background(), cfg, "/absent-candidate-master"); !errors.Is(err, ErrHAGroupKeyRotationRequired) {
		t.Fatal("single member File rotation reached old Metadata/key replacement", err)
	}
	cfg.Storage.MasterKey.Mode = config.MasterKeyModeKeySlots
	if _, err := RotateKMSMasterKey(context.Background(), cfg, "whole-group-required"); !errors.Is(err, ErrHAGroupKeyRotationRequired) {
		t.Fatal("single member KMS rotation reached upstream", err)
	}
	after, err := os.ReadFile(cfg.ReplicationStatePath())
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("single member rejection changed actual Member", err)
	}
	if _, _, err := readMemberState(cfg, old); err != nil {
		t.Fatal("single member rejection damaged original authority", err)
	}
	if _, err := os.Lstat(haGroupClusterKeyPath(cfg)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("single member rejection installed cluster-key candidate", err)
	}
}

func TestHAGroupClusterKeyCandidateAuthenticatesOriginalMemberAfterDEKChange(t *testing.T) {
	cfg, original, old, next, key := haGroupKeyCandidateFixture(t)
	before, err := os.ReadFile(cfg.ReplicationStatePath())
	if err != nil {
		t.Fatal(err)
	}
	if actual, actualKey, err := readMemberState(cfg, old); err != nil || !reflect.DeepEqual(actual, original) || actualKey != key {
		t.Fatal("original Member/key changed before candidate publication", actual, err)
	}
	if _, _, err := readMemberState(cfg, next); err == nil {
		t.Fatal("new DEK authenticated original Member without stable key carrier")
	}
	raw, err := encodeHAGroupClusterKeyCandidate(cfg, key, next)
	if err != nil || len(raw) != haGroupClusterKeyBytes {
		t.Fatal(err)
	}
	if err := os.WriteFile(haGroupClusterKeyPath(cfg), raw, 0600); err != nil {
		t.Fatal(err)
	}
	actual, actualKey, err := readMemberState(cfg, next)
	if err != nil || !reflect.DeepEqual(actual, original) || actualKey != key {
		t.Fatal("key candidate altered actual role/term/ordering/prefix authority", actual, err)
	}
	if _, _, err := readMemberState(cfg, old); err == nil {
		t.Fatal("committed candidate silently fell back to old DEK")
	}
	after, err := os.ReadFile(cfg.ReplicationStatePath())
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("key handoff rewrote original Member bytes", err)
	}
}

func TestHAGroupClusterKeyCandidateRejectsTamperForeignIdentityAndWrongNativeMAC(t *testing.T) {
	for _, failure := range []string{"ciphertext", "length", "public", "symlink", "foreign-config", "wrong-native-key", "native-mac", "incarnation"} {
		t.Run(failure, func(t *testing.T) {
			cfg, state, _, next, key := haGroupKeyCandidateFixture(t)
			raw, err := encodeHAGroupClusterKeyCandidate(cfg, key, next)
			if err != nil {
				t.Fatal(err)
			}
			switch failure {
			case "ciphertext":
				raw[len(raw)-1] ^= 1
			case "length":
				raw = append(raw, 0)
			case "wrong-native-key":
				wrong := sha256.Sum256([]byte("foreign cluster key"))
				if _, err := encodeHAGroupClusterKeyCandidate(cfg, wrong, next); err == nil {
					t.Fatal("creation accepted unauthenticated actual Member")
				}
				if err := replication.WriteState(cfg.ReplicationStatePath(), state, wrong[:]); err != nil {
					t.Fatal(err)
				}
			case "native-mac":
				native, err := os.ReadFile(cfg.ReplicationStatePath())
				if err != nil {
					t.Fatal(err)
				}
				native = bytes.Replace(native, []byte("replica"), []byte("primary"), 1)
				if err := os.WriteFile(cfg.ReplicationStatePath(), native, 0600); err != nil {
					t.Fatal(err)
				}
			case "incarnation":
				state.Incarnation = "other_incarnation"
				if err := replication.WriteState(cfg.ReplicationStatePath(), state, key[:]); err != nil {
					t.Fatal(err)
				}
			}
			path := haGroupClusterKeyPath(cfg)
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			if failure == "public" {
				if err := os.Chmod(path, 0644); err != nil {
					t.Fatal(err)
				}
			}
			if failure == "symlink" {
				if err := os.Rename(path, path+".foreign"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path+".foreign", path); err != nil {
					t.Fatal(err)
				}
			}
			if failure == "foreign-config" {
				cfg.Replication.NodeID = "foreign-node"
			}
			if _, actualKey, err := readMemberState(cfg, next); err == nil || actualKey != ([32]byte{}) {
				t.Fatal("candidate granted Member authority or leaked candidate on rejection", err)
			}
			if _, err := os.Lstat(path); err != nil {
				t.Fatal("failed read removed candidate/foreign path", err)
			}
		})
	}
}
