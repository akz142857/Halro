package scratchstate

import "bytes"

// A receipt exists only on this mutation's recursive call stack. Its actual
// ReadAt, MAC/hash and key-blob verification already happened in readChild.
// The parent consumes it once instead of repeating that same immediate child
// read in writeNode. Nothing is saved in Store or lent across Get/Put jobs.
// Untouched children and every rotation retain the ordinary actual FD reads.
type childReceipt struct {
	source    *Store
	operation uint64
	root      link
	child     node
	consumed  bool
}

func (s *Store) startMutation() error {
	if s.operation == ^uint64(0) {
		return ErrLimit
	}
	s.operation++
	return nil
}
func (s *Store) readChild(root link) (childReceipt, error) {
	child, err := s.readNode(root)
	if err != nil {
		return childReceipt{}, err
	}
	return childReceipt{source: s, operation: s.operation, root: root, child: child}, nil
}
func (s *Store) childRowsWithReceipt(root link, parent node, left bool, checked *childReceipt) (uint64, error) {
	if checked == nil || root != checked.root {
		return s.childRows(root, parent, left)
	}
	child := checked.child
	// These are the same priority/range/count checks as an independently read
	// child. Parent membership and operation/owner lifetime are checked before
	// either receipt can be consumed; no caller node/header creates a receipt.
	if child.priority < parent.priority || left && bytes.Compare(child.key, parent.key) >= 0 || !left && bytes.Compare(child.key, parent.key) <= 0 || child.rows == 0 || child.rows > s.next[0]-1 {
		return 0, ErrCorrupt
	}
	return child.rows, nil
}
