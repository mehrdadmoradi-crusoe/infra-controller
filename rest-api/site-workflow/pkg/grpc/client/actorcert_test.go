// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NVIDIA/infra-controller/rest-api/common/pkg/coreproxy"
)

// testActorCA is a self-signed CA standing in for nico-actor-ca. Returned as
// PEM, the way the files arrive from the mounted secret.
func testActorCA(t *testing.T, cn string) (certPEM, keyPEM []byte, cert *x509.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	cert, err = x509.ParseCertificate(der)
	require.NoError(t, err)
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
		cert
}

// The certificate has to say exactly what Core will read from it: the user in
// CN, org in O, group in OU, issued by the CA whose CN Core has been told to
// treat as an external issuer, and no SPIFFE URI that would send Core down the
// service-identity path instead.
func TestMintActorCertificateNamesTheActorTheWayCoreReadsIt(t *testing.T) {
	caPEM, keyPEM, ca := testActorCA(t, "test-actor-ca")

	cert, err := MintActorCertificate(caPEM, keyPEM, coreproxy.Actor{
		User: "alice@example.com", Org: "acme", Group: "PROVIDER_ADMIN",
	}, ActorCertTTL)
	require.NoError(t, err)
	require.NotNil(t, cert.Leaf)

	leaf := cert.Leaf
	assert.Equal(t, "alice@example.com", leaf.Subject.CommonName)
	assert.Equal(t, []string{"acme"}, leaf.Subject.Organization)
	assert.Equal(t, []string{"PROVIDER_ADMIN"}, leaf.Subject.OrganizationalUnit)
	assert.Equal(t, "test-actor-ca", leaf.Issuer.CommonName)

	assert.Empty(t, leaf.URIs, "a SPIFFE URI would make Core treat this as a service, not a user")
	assert.Empty(t, leaf.DNSNames)
	assert.False(t, leaf.IsCA)
	assert.Contains(t, leaf.ExtKeyUsage, x509.ExtKeyUsageClientAuth)

	// Short-lived, and already valid despite a Core clock a little behind.
	assert.WithinDuration(t, time.Now().Add(ActorCertTTL), leaf.NotAfter, 5*time.Second)
	assert.True(t, leaf.NotBefore.Before(time.Now()))

	// The CA travels with the leaf so the chain reaches the root Core holds.
	require.Len(t, cert.Certificate, 2)
	assert.Equal(t, ca.Raw, cert.Certificate[1])

	roots := x509.NewCertPool()
	roots.AddCert(ca)
	_, err = leaf.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
	assert.NoError(t, err, "the minted chain must verify against the CA")
}

// Org and group are informational to Core and may be absent; the user is not.
func TestMintActorCertificateRequiresAUser(t *testing.T) {
	caPEM, keyPEM, _ := testActorCA(t, "test-actor-ca")

	_, err := MintActorCertificate(caPEM, keyPEM, coreproxy.Actor{Org: "acme"}, ActorCertTTL)
	assert.ErrorIs(t, err, ErrActorUserEmpty)

	cert, err := MintActorCertificate(caPEM, keyPEM, coreproxy.Actor{User: "bob"}, ActorCertTTL)
	require.NoError(t, err)
	assert.Equal(t, "bob", cert.Leaf.Subject.CommonName)
	assert.Empty(t, cert.Leaf.Subject.Organization)
	assert.Empty(t, cert.Leaf.Subject.OrganizationalUnit)
}

// A CA that is not a CA, or material that is not PEM, is refused before any
// signing happens rather than producing a certificate Core would reject.
func TestMintActorCertificateRefusesBadCAMaterial(t *testing.T) {
	caPEM, keyPEM, _ := testActorCA(t, "test-actor-ca")

	_, err := MintActorCertificate([]byte("not pem"), keyPEM, coreproxy.Actor{User: "x"}, ActorCertTTL)
	assert.ErrorContains(t, err, "not PEM")

	_, err = MintActorCertificate(caPEM, []byte("not pem"), coreproxy.Actor{User: "x"}, ActorCertTTL)
	assert.ErrorContains(t, err, "not PEM")

	// A minted leaf used as the CA: valid PEM, but not a CA.
	leaf, err := MintActorCertificate(caPEM, keyPEM, coreproxy.Actor{User: "x"}, ActorCertTTL)
	require.NoError(t, err)
	leafPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leaf.Certificate[0]})
	_, err = MintActorCertificate(leafPEM, keyPEM, coreproxy.Actor{User: "x"}, ActorCertTTL)
	assert.ErrorContains(t, err, "not a CA")
}
