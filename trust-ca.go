//go:build !windows
// +build !windows

package main

import (
	"crypto/x509"
	"embed"
	"io"
	"log"
	"path"
)

//go:embed trust-ca/*.pem
var additionalFs embed.FS

var (
	rootCAs *x509.CertPool
)

func init() {
	rootCAs, _ := x509.SystemCertPool()
	if rootCAs == nil {
		return
	}

	var walk func(dir string)
	walk = func(dir string) {
		fileList, err := additionalFs.ReadDir(dir)
		if err != nil {
			panic(err)
		}

		for _, entry := range fileList {
			path := path.Join(dir, entry.Name())

			if entry.IsDir() {
				walk(path)
			}

			pemData, err := additionalFs.ReadFile(path)
			if err != nil && err != io.EOF {
				panic(err)
			}

			ok := rootCAs.AppendCertsFromPEM(pemData)
			if !ok {
				log.Printf("No certs appended (%s)\n", entry.Name())
			}
		}
	}

	walk("trust-ca")
}
