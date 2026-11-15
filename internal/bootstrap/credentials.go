package bootstrap

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
)

// The files libvirt reads from its PKI directory, and the keys the provider
// expects in the hypervisor credentials Secret.
const (
	CACertFile     = "cacert.pem"
	ClientCertFile = "clientcert.pem"
	ClientKeyFile  = "clientkey.pem"
)

// Credentials are a hypervisor's TLS client credentials. Their contents are
// never printed or logged.
type Credentials struct {
	caCert, clientCert, clientKey []byte
}

// Data returns the Secret data.
func (c *Credentials) Data() map[string][]byte {
	return map[string][]byte{
		CACertFile:     c.caCert,
		ClientCertFile: c.clientCert,
		ClientKeyFile:  c.clientKey,
	}
}

// LoadCredentials reads cacert.pem, clientcert.pem and clientkey.pem from dir
// and checks they are what the provider will need: PEM certificates, and a
// client key that matches the client certificate. It checks shape only;
// whether the hypervisor accepts them is hydra validate's job.
func LoadCredentials(dir string) (*Credentials, error) {
	read := func(name string) ([]byte, error) {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, fmt.Errorf("hypervisor credentials: %w", err)
		}
		return b, nil
	}
	var c Credentials
	var err error
	if c.caCert, err = read(CACertFile); err != nil {
		return nil, err
	}
	if c.clientCert, err = read(ClientCertFile); err != nil {
		return nil, err
	}
	if c.clientKey, err = read(ClientKeyFile); err != nil {
		return nil, err
	}
	if err := isCertificate(c.caCert); err != nil {
		return nil, fmt.Errorf("hypervisor credentials: %s: %w", CACertFile, err)
	}
	if err := isCertificate(c.clientCert); err != nil {
		return nil, fmt.Errorf("hypervisor credentials: %s: %w", ClientCertFile, err)
	}
	if _, err := tls.X509KeyPair(c.clientCert, c.clientKey); err != nil {
		// The tls error text never includes key material.
		return nil, fmt.Errorf("hypervisor credentials: %s does not match %s: %w", ClientKeyFile, ClientCertFile, err)
	}
	return &c, nil
}

func isCertificate(b []byte) error {
	block, _ := pem.Decode(b)
	if block == nil || block.Type != "CERTIFICATE" {
		return fmt.Errorf("not a PEM certificate")
	}
	if _, err := x509.ParseCertificate(block.Bytes); err != nil {
		return fmt.Errorf("unparseable certificate: %w", err)
	}
	return nil
}
