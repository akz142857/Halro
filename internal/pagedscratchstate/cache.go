package pagedscratchstate

import "errors"

func (s *Store) flushCache(force bool) error {
	if !force && s.unsynced < cacheWindowBytes {
		return nil
	}
	if err := s.verifyNames(); err != nil {
		return err
	}
	if err := s.flushDirtyBank(); err != nil {
		return err
	}
	for i, f := range s.files {
		if err := s.owners[i].VerifyFile(f); err != nil {
			return err
		}
		if err := f.Sync(); err != nil {
			return err
		}
	}
	for _, f := range s.files {
		info, err := f.Stat()
		if err != nil {
			return err
		}
		if err := retireCacheWindow(f, info.Size()); err != nil {
			s.cacheErr = errors.Join(s.cacheErr, err)
			return err
		}
	}
	s.unsynced = 0
	s.readBytes = 0
	return nil
}
