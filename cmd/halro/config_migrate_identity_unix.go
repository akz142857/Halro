//go:build unix

package main

import (
	"os"
	"syscall"
)

type fileIdentity struct {
	uid int
	gid int
}

func configFileIdentity(info os.FileInfo) fileIdentity {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fileIdentity{uid: -1, gid: -1}
	}
	return fileIdentity{uid: int(stat.Uid), gid: int(stat.Gid)}
}

func applyConfigFileIdentity(file *os.File, identity fileIdentity) error {
	if identity.uid < 0 || identity.gid < 0 {
		return nil
	}
	return file.Chown(identity.uid, identity.gid)
}
