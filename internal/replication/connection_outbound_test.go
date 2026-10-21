package replication

import (
	"bytes"
	"errors"
	"testing"
)

type outboundTestSender struct {
	errors map[string]error
	peers  []string
	values [][]byte
}

func (s *outboundTestSender) Send(peer string, encoded []byte) error {
	s.peers = append(s.peers, peer)
	s.values = append(s.values, append([]byte(nil), encoded...))
	return s.errors[peer]
}

func TestConnectionOutboundRequiresOneReplicaEnqueueAndAttemptsAll(t *testing.T) {
	disconnected := errors.New("disconnected")
	sender := &outboundTestSender{errors: map[string]error{"halro-2": disconnected}}
	outbound, err := NewConnectionOutbound([]string{"halro-2", "halro-1"}, sender)
	if err != nil {
		t.Fatal(err)
	}
	record := []byte("record")
	if err := outbound.QueueFrame(1, record); err != nil {
		t.Fatalf("one healthy Replica did not satisfy enqueue quorum: %v", err)
	}
	record[0] = 'X'
	if len(sender.peers) != 2 || sender.peers[0] != "halro-1" || sender.peers[1] != "halro-2" ||
		!bytes.Equal(sender.values[0], []byte("record")) || !bytes.Equal(sender.values[1], []byte("record")) {
		t.Fatalf("broadcast peers=%v values=%q", sender.peers, sender.values)
	}
}

func TestConnectionOutboundFailsWhenNoReplicaCanQueue(t *testing.T) {
	disconnected := errors.New("disconnected")
	sender := &outboundTestSender{errors: map[string]error{"halro-1": disconnected, "halro-2": disconnected}}
	outbound, err := NewConnectionOutbound([]string{"halro-1", "halro-2"}, sender)
	if err != nil {
		t.Fatal(err)
	}
	if err := outbound.QueueCommitNotice(1, []byte("notice")); !errors.Is(err, disconnected) {
		t.Fatalf("all-peer failure=%v", err)
	}
}
