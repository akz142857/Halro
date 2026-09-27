//go:build !unix

package main

import "os"

type fileIdentity struct{}

func configFileIdentity(os.FileInfo) fileIdentity          { return fileIdentity{} }
func applyConfigFileIdentity(*os.File, fileIdentity) error { return nil }
