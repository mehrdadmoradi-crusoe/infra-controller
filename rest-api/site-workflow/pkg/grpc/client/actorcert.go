// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/NVIDIA/infra-controller/rest-api/common/pkg/coreproxy"
)

// Core authenticates external humans by client certificate: a certificate
// whose issuer CN it has been told to treat as an external issuer is read as
// an external user, with CN as the user, O as the org and OU as the group. The
// admin CLI works this way. Requests proxied by the site do not, because the
// site presents its own service certificate, and for the few Core methods that
// record who asked -- requesting, approving and applying a Redfish action --
// Core refuses a caller it cannot name.
//
// MintActorCertificate is how the site speaks for a named user instead. It
// issues a short-lived leaf for the actor from a CA the Core has been told to
// trust as an external issuer, and the proxy presents that leaf for the one
// call. Core then sees exactly what it would see from that person's own
// certificate.
//
// The leaf carries no SPIFFE URI on purpose. Core tries SPIFFE first and falls
// through to the external-user path only when that fails, so a URI here would
// make the certificate a malformed service identity rather than a user.

// ActorCertTTL is how long a minted actor certificate is valid for. It is
// presented once, on one connection, immediately after minting; the margin
// covers clock skew between the site and Core and nothing else.
const ActorCertTTL = 5 * time.Minute

// actorCertBackdate absorbs a Core clock slightly behind the site's.
const actorCertBackdate = time.Minute

// ErrActorUserEmpty is returned when the actor names no user. Core would refuse
// the certificate anyway, and a nameless actor is the one thing this exists to
// prevent, so it is refused before anything is signed.
var ErrActorUserEmpty = errors.New("actor user must not be empty")

// MintActorCertificate issues a client certificate naming actor, signed by the
// CA in caCertPEM/caKeyPEM, valid for ttl. The returned certificate carries
// the CA as its intermediate so the chain reaches whatever root the CA itself
// was issued from.
func MintActorCertificate(caCertPEM, caKeyPEM []byte, actor coreproxy.Actor, ttl time.Duration) (tls.Certificate, error) {
	if actor.User == "" {
		return tls.Certificate{}, ErrActorUserEmpty
	}

	caCert, caKey, err := parseActorCA(caCertPEM, caKeyPEM)
	if err != nil {
		return tls.Certificate{}, err
	}

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("generate actor key: %w", err)
	}

	// 127 bits keeps the serial positive and comfortably unique per mint.
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("generate serial: %w", err)
	}

	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: actor.User},
		NotBefore:    now.Add(-actorCertBackdate),
		NotAfter:     now.Add(ttl),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		// A leaf, and says so; a CA that also happened to mint leaves would be
		// a different and worse thing.
		BasicConstraintsValid: true,
		IsCA:                  false,
	}
	if actor.Org != "" {
		template.Subject.Organization = []string{actor.Org}
	}
	if actor.Group != "" {
		template.Subject.OrganizationalUnit = []string{actor.Group}
	}

	leafDER, err := x509.CreateCertificate(rand.Reader, template, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("sign actor certificate: %w", err)
	}
	leaf, err := x509.ParseCertificate(leafDER)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("parse minted certificate: %w", err)
	}

	return tls.Certificate{
		Certificate: [][]byte{leafDER, caCert.Raw},
		PrivateKey:  leafKey,
		Leaf:        leaf,
	}, nil
}

// parseActorCA reads the CA certificate and key from PEM. The key may be
// PKCS#8, SEC 1 (EC) or PKCS#1, which covers what cert-manager and openssl
// produce.
func parseActorCA(caCertPEM, caKeyPEM []byte) (*x509.Certificate, crypto.Signer, error) {
	certBlock, _ := pem.Decode(caCertPEM)
	if certBlock == nil {
		return nil, nil, errors.New("actor CA certificate is not PEM")
	}
	caCert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("parse actor CA certificate: %w", err)
	}
	if !caCert.IsCA {
		return nil, nil, errors.New("actor CA certificate is not a CA")
	}

	keyBlock, _ := pem.Decode(caKeyPEM)
	if keyBlock == nil {
		return nil, nil, errors.New("actor CA key is not PEM")
	}
	var key any
	switch keyBlock.Type {
	case "EC PRIVATE KEY":
		key, err = x509.ParseECPrivateKey(keyBlock.Bytes)
	case "RSA PRIVATE KEY":
		key, err = x509.ParsePKCS1PrivateKey(keyBlock.Bytes)
	default:
		key, err = x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("parse actor CA key: %w", err)
	}
	signer, ok := key.(crypto.Signer)
	if !ok {
		return nil, nil, errors.New("actor CA key cannot sign")
	}
	return caCert, signer, nil
}
