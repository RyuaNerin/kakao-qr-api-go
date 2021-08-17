//go:build windows
// +build windows

// Windows 에서는 CA가 Trusted 에 이미 추가되어 있으므로 필요 없어서 무방.

package main

import (
	"crypto/x509"
)

var (
	rootCAs *x509.CertPool
)
