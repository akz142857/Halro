package replication

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/replicationlimits"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

func portableReceiptBudget(t *testing.T) *resourcebudget.Manager {
	t.Helper()
	limits := resourcebudget.Limits{Total: resourcebudget.Cost{RAM: 256 << 20, Disk: 16 << 20}, MaxClaims: 64, MaxOwners: 16, MaxPressureAge: time.Hour, RecoveryReserve: resourcebudget.Cost{RAM: 128 << 20}, RecoveryClaims: 16}
	limits.Classes[resourcebudget.IO] = resourcebudget.Cost{RAM: 128 << 20, Disk: 8 << 20}
	limits.Classes[resourcebudget.Background] = resourcebudget.Cost{RAM: 64 << 20, Disk: 8 << 20}
	limits.Classes[resourcebudget.Recovery] = limits.RecoveryReserve
	limits.Classes[resourcebudget.Control] = resourcebudget.Cost{RAM: 64 * resourcebudget.ClaimRAMBytes}
	m, err := resourcebudget.New(limits)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.SetPressure(resourcebudget.Healthy); err != nil {
		t.Fatal(err)
	}
	return m
}

type portableReceiptNativeOptions struct {
	LedgerPath, AuditPath, GovernancePath, MetadataPath, BaselinePath string
	LedgerKey, AuditKey, GovernanceKey, MetadataKey, BaselineKey      []byte
}
type portableReceiptSourceInput struct {
	Options         portableReceiptNativeOptions
	Ordering, State string
	Carrier, Key    []byte
	Phase, Ready    string
}

func preparePortableReceiptSourceInput(t *testing.T) portableReceiptSourceInput {
	t.Helper()
	s, j, a, options, _, signed := metadataBoundaryFixture(t)
	metadata, err := a.Descriptor().MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	n := a.Claims().Index
	if _, err = j.Append(OrderingRecord{Kind: KindMetadataBaseline, Index: n + 1, Term: a.Claims().Term, ConfirmedIndex: n, Metadata: metadata, FrameDigest: sha256.Sum256(metadata)}); err != nil {
		t.Fatal(err)
	}
	if err = s.ActivateMetadataBaseline(j, n+1); err != nil {
		t.Fatal(err)
	}
	index, term, mac := j.Head()
	cursors, err := j.StoreCursors()
	if err != nil {
		t.Fatal(err)
	}
	member := validMemberState()
	member.NodeID = "halro-1"
	member.Role = RoleReplica
	member.Peers[1].Name = "halro-0"
	member.Term, member.PromisedTerm = term, term
	member.DurableIndex, member.ConfirmedIndex, member.AppliedIndex = index, index, index
	member.OrderingHeadMAC = mac
	member.Projection = ProjectionState{Index: index, MetadataEpoch: cursors[3].Generation, MetadataSequence: cursors[3].Sequence}
	state := filepath.Join(t.TempDir(), "state.json")
	key := []byte("0123456789abcdef0123456789abcdef")
	if err = WriteState(state, member, key); err != nil {
		t.Fatal(err)
	}
	order := j.file.Name()
	if err = j.Close(); err != nil {
		t.Fatal(err)
	}
	// Authority and Manager are reconstructed from the actual independently
	// signed source carrier and original Member in the fresh child process.
	options.MetadataBaseline = &MetadataBaselineSourceOptions{Path: options.MetadataBaseline.Path, Key: options.MetadataBaseline.Key}
	return portableReceiptSourceInput{Options: portableReceiptNativeOptions{LedgerPath: options.LedgerPath, AuditPath: options.AuditPath, GovernancePath: options.GovernancePath, MetadataPath: options.MetadataPath, BaselinePath: options.MetadataBaseline.Path, LedgerKey: options.LedgerKey, AuditKey: options.AuditKey, GovernanceKey: options.GovernanceKey, MetadataKey: options.MetadataKey, BaselineKey: options.MetadataBaseline.Key}, Ordering: order, State: state, Carrier: signed, Key: key}
}
func openPortableReceiptSourceInput(t *testing.T, input portableReceiptSourceInput) (*StatePublisher, *OrderingJournal, *NativeSource, *MetadataBaselineAuthority, *resourcebudget.Manager, resourcebudget.Permit) {
	t.Helper()
	m := portableReceiptBudget(t)
	state, err := ReadState(input.State, input.Key)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := QualifyMetadataBaselineCarrier(input.Carrier, input.Key, state)
	if err != nil {
		t.Fatal(err)
	}
	options := NativeSourceOptions{LedgerPath: input.Options.LedgerPath, AuditPath: input.Options.AuditPath, GovernancePath: input.Options.GovernancePath, MetadataPath: input.Options.MetadataPath, LedgerKey: input.Options.LedgerKey, AuditKey: input.Options.AuditKey, GovernanceKey: input.Options.GovernanceKey, MetadataKey: input.Options.MetadataKey}
	options.MetadataBaseline = &MetadataBaselineSourceOptions{Path: input.Options.BaselinePath, Key: input.Options.BaselineKey, Authority: authority}
	options.ReaderBudget = m
	options.ReaderRAMBytes = replicationlimits.MinNativeReaderRAMBytes + MetadataBaselineReaderRAMBytes
	source, err := NewNativeSource(options)
	if err != nil {
		t.Fatal(err)
	}
	j, err := OpenExistingOrderingJournal(input.Ordering, input.Key, state.ClusterID, state.Incarnation, state.DurableIndex, state.OrderingHeadMAC)
	if err != nil {
		t.Fatal(err)
	}
	if err = source.ActivateMetadataBaseline(j, authority.Claims().Index+1); err != nil {
		t.Fatal(err)
	}
	p, err := NewStatePublisher(input.State, input.Key, state)
	if err != nil {
		t.Fatal(err)
	}
	fee, err := m.TryAcquire(resourcebudget.Background, resourcebudget.Cost{Disk: PortableReceivedBankDiskBytes})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		p.Close()
		if err := errors.Join(p.PhysicalCloseError(), j.Close(), source.Close(), fee.Release()); err != nil {
			t.Error(err)
		}
	})
	return p, j, source, authority, m, fee
}
func TestPortableReceivedActualCreationSIGKILLAndOpaqueRestart(t *testing.T) {
	for _, phase := range []string{"received-created", "received-header-written", "received-synced"} {
		t.Run(phase, func(t *testing.T) {
			input := preparePortableReceiptSourceInput(t)
			input.Phase = phase
			input.Ready = filepath.Join(t.TempDir(), "ready")
			raw, err := json.Marshal(input)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "input.json")
			if err = os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			child := exec.Command(os.Args[0], "-test.run=^TestPortableReceivedActualCreationChild$", "-test.count=1", "-test.timeout=30s")
			child.Env = append(os.Environ(), "HALRO_RECEIVED_CREATE_INPUT="+path)
			var output bytes.Buffer
			child.Stdout = &output
			child.Stderr = &output
			if err = child.Start(); err != nil {
				t.Fatal(err)
			}
			joined := false
			defer func() {
				if !joined {
					child.Process.Kill()
					child.Wait()
				}
			}()
			until := time.Now().Add(15 * time.Second)
			for {
				if _, err = os.Stat(input.Ready); err == nil {
					break
				}
				if time.Now().After(until) {
					t.Fatal("real creation phase absent", output.String())
				}
				time.Sleep(5 * time.Millisecond)
			}
			if err = child.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			err = child.Wait()
			joined = true
			var exit *exec.ExitError
			if !errors.As(err, &exit) {
				t.Fatal(err, output.String())
			}
			status, ok := exit.Sys().(syscall.WaitStatus)
			if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
				t.Fatal(status)
			}
			p, j, s, a, m, fee := openPortableReceiptSourceInput(t, input)
			before := p.Snapshot()
			bank := portableReceivedPath(p)
			raw, err = os.ReadFile(bank)
			if err != nil {
				t.Fatal(err)
			}
			original := bytes.Clone(raw)
			if phase == "received-created" {
				if len(raw) != 0 {
					t.Fatal("actual exclusive-create window changed", len(raw))
				}
				if _, err = p.PreparePortableReceivedPublication(context.Background(), j, s, a, m, fee); err == nil {
					t.Fatal("incomplete candidate automatically became permission")
				}
			}
			if _, err = p.PreparePortableReceivedPublicationWithOptions(context.Background(), j, s, a, m, fee, PortableReceivedPublicationOptions{Restart: true}); err != nil {
				t.Fatal("explicit independent full source restart", err)
			}
			if !sameHAPeerLeadershipMember(before, p.Snapshot()) {
				t.Fatal("restart changed actual progress")
			}
			if phase == "received-created" {
				retained, err := os.ReadFile(portableReceivedOpaquePath(bank, 0))
				if err != nil || !bytes.Equal(retained, original) {
					t.Fatal("opaque original was deleted/changed", err)
				}
			}
			if info, err := os.Stat(bank); err != nil || info.Size() != PortableReceivedDiskBytes {
				t.Fatal(info, err)
			}
		})
	}
}
func TestPortableReceivedActualCreationChild(t *testing.T) {
	path := os.Getenv("HALRO_RECEIVED_CREATE_INPUT")
	if path == "" {
		t.Skip("true process helper")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var input portableReceiptSourceInput
	if err = json.Unmarshal(raw, &input); err != nil {
		t.Fatal(err)
	}
	p, j, s, a, m, fee := openPortableReceiptSourceInput(t, input)
	_, err = p.PreparePortableReceivedPublicationWithOptions(context.Background(), j, s, a, m, fee, PortableReceivedPublicationOptions{phase: func(phase string) error {
		if phase == input.Phase {
			if err := os.WriteFile(input.Ready, []byte(phase), 0600); err != nil {
				return err
			}
			select {}
		}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Fatal("parent must SIGKILL before callback returns")
}

func TestPortableReceivedOpaqueBoundedInventoryForeignAndExhaustion(t *testing.T) {
	input := preparePortableReceiptSourceInput(t)
	for attempt := 0; attempt < portableReceivedOpaqueSlots+1; attempt++ {
		p, j, s, a, m, fee := openPortableReceiptSourceInput(t, input)
		bank := portableReceivedPath(p)
		if err := os.WriteFile(bank, []byte("unqualified interrupted contents"), 0600); err != nil {
			t.Fatal(err)
		}
		memberBefore, err := os.ReadFile(p.path)
		if err != nil {
			t.Fatal(err)
		}
		journalBefore, err := os.ReadFile(j.file.Name())
		if err != nil {
			t.Fatal(err)
		}
		used := m.Snapshot().Used
		_, err = p.PreparePortableReceivedPublicationWithOptions(context.Background(), j, s, a, m, fee, PortableReceivedPublicationOptions{Restart: true})
		if attempt < portableReceivedOpaqueSlots {
			if err != nil {
				t.Fatal(attempt, err)
			}
		} else if !errors.Is(err, ErrReceivedOpaqueSlots) || m.Snapshot().Used != used {
			t.Fatal("fifth retry leaked/moved/created", err, m.Snapshot())
		}
		actual, err := os.ReadFile(p.path)
		if err != nil || !bytes.Equal(actual, memberBefore) {
			t.Fatal("opaque restart changed Member", err)
		}
		actual, err = os.ReadFile(j.file.Name())
		if err != nil || !bytes.Equal(actual, journalBefore) {
			t.Fatal("opaque restart changed Ordering", err)
		}
		p.Close()
		if err = errors.Join(p.PhysicalCloseError(), j.Close(), s.Close()); err != nil {
			t.Fatal(err)
		}
	}
	p, j, s, a, m, fee := openPortableReceiptSourceInput(t, input)
	bank := portableReceivedPath(p)
	if err := os.Remove(bank); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(t.TempDir(), "foreign")
	if err := os.WriteFile(foreign, []byte("foreign"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(foreign, bank); err != nil {
		t.Fatal(err)
	}
	used := m.Snapshot().Used
	if _, err := p.PreparePortableReceivedPublicationWithOptions(context.Background(), j, s, a, m, fee, PortableReceivedPublicationOptions{Restart: true}); err == nil || m.Snapshot().Used != used {
		t.Fatal("symlink or unbounded inventory accepted", err)
	}
	actual, err := os.ReadFile(foreign)
	if err != nil || string(actual) != "foreign" {
		t.Fatal("foreign modified", err)
	}
	if !errors.Is(p.PhysicalCloseError(), ownedfile.ErrRetirement) && p.PhysicalCloseError() != nil {
		t.Fatal(p.PhysicalCloseError())
	}
}

func TestPortableReceivedActualReceiverPreNativeAndForeignRefusals(t *testing.T) {
	input := preparePortableReceiptSourceInput(t)
	p, j, s, a, m, fee := openPortableReceiptSourceInput(t, input)
	foreign := portableReceiptBudget(t)
	before := m.Snapshot().Used
	if _, err := p.PreparePortableReceivedPublication(context.Background(), j, s, a, foreign, fee); err == nil || m.Snapshot().Used != before {
		t.Fatal("foreign Manager borrowed original source", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.PreparePortableReceivedPublication(canceled, j, s, a, m, fee); err == nil || m.Snapshot().Used != before {
		t.Fatal("canceled owner allocated receipt", err)
	}
	if _, err := os.Lstat(portableReceivedPath(p)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed source preflight touched receipt", err)
	}
	o, err := p.PreparePortableReceivedPublication(context.Background(), j, s, a, m, fee)
	if err != nil {
		t.Fatal(err)
	}
	old := p.Snapshot()
	writes := 0
	receiver, err := NewReplicaReceiver(old.ClusterID, old.Incarnation, old.NodeID, a.CurrentPrimary(), old.Term, old.PromisedTerm, old.ConfirmedIndex, old.AppliedIndex, j, receivedBeforeNativeSink{visit: func(frame Frame) error {
		raw, _, err := readPortableReceivedReceiptIdentity(portableReceivedPath(p))
		if err != nil {
			return err
		}
		intent, err := decodeReceivedIntent(raw, frame.Index, o.header.intentHeader(), p.key[:])
		if err != nil {
			return err
		}
		current := p.Snapshot()
		digest, err := memberReceiptDigest(current, p.key[:])
		if err != nil {
			return err
		}
		index, _, mac := j.Head()
		record, err := UnmarshalOrderingRecord(intent.Record, j.key[:], old.ClusterID, old.Incarnation, mac)
		if err != nil || intent.Before != digest || intent.Primary != a.CurrentPrimary() || record.Index != index+1 || record.Index != frame.Index {
			return errors.Join(ErrMemberAuthority, err)
		}
		writes++
		return nil
	}}, ReplicaReceiverOptions{PersistState: p.PublishReplica, Projection: old.Projection})
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := (SchemaBoundaryMetadata{From: 45, To: 46}).MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	frame := Frame{Kind: KindSchemaBoundary, ClusterID: old.ClusterID, Incarnation: old.Incarnation, Term: old.Term, Index: old.DurableIndex + 1, ConfirmedIndex: old.ConfirmedIndex, Metadata: metadata}
	encoded, err := frame.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = receiver.Receive(encoded); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(portableReceivedPath(p))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = receiver.Receive(encoded); err != nil {
		t.Fatal(err)
	}
	actual, err := os.ReadFile(portableReceivedPath(p))
	if err != nil || !bytes.Equal(raw, actual) || writes != 1 {
		t.Fatal("duplicate reissued acceptance", writes, err)
	}
	if err = p.ValidatePortableReceivedPublication(context.Background(), j, m); err != nil {
		t.Fatal(err)
	}
	p.Close()
	if err = errors.Join(p.PhysicalCloseError(), j.Close(), s.Close()); err != nil {
		t.Fatal(err)
	}
	// Even a structurally complete, same-length candidate cannot supply its own
	// MAC, Node identity or birth grant on the next independently qualified open.
	raw[100] ^= 1
	if err = os.WriteFile(portableReceivedPath(p), raw, 0600); err != nil {
		t.Fatal(err)
	}
	p2, j2, s2, a2, m2, fee2 := openPortableReceiptSourceInput(t, input)
	before = m2.Snapshot().Used
	if _, err = p2.PreparePortableReceivedPublication(context.Background(), j2, s2, a2, m2, fee2); err == nil || m2.Snapshot().Used != before {
		t.Fatal("tampered independent-header candidate accepted/leaked", err)
	}
}
