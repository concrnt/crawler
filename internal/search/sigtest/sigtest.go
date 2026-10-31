// Package sigtest signs concrnt documents for tests. The crawler verifies
// every scraped document, so fixtures need real secp256k1 identities; the
// helpers here mirror concrnt's own (test-only, hence unimportable) signing
// helpers and build the proof shapes the crawler accepts and rejects.
package sigtest

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/concrnt/concrnt"
	"github.com/concrnt/concrnt/schemas"
)

// Identity is a secp256k1 key pair and the CCID derived from it.
type Identity struct {
	CCID    string
	PrivKey string // hex
}

// MustIdentity derives the identity of a fixed private key, so fixtures can
// be package-level vars with CCIDs that are stable across runs. It panics on
// a bad key.
func MustIdentity(privHex string) Identity {
	ccid, err := concrnt.PrivKeyToAddr(privHex, "con")
	if err != nil {
		panic(err)
	}
	return Identity{CCID: ccid, PrivKey: privHex}
}

// NewIdentity generates a random identity.
func NewIdentity(t testing.TB) Identity {
	t.Helper()
	priv := make([]byte, 32)
	if _, err := rand.Read(priv); err != nil {
		t.Fatal(err)
	}
	privHex := hex.EncodeToString(priv)
	ccid, err := concrnt.PrivKeyToAddr(privHex, "con")
	if err != nil {
		t.Fatal(err)
	}
	return Identity{CCID: ccid, PrivKey: privHex}
}

func marshal(t testing.TB, doc any) []byte {
	t.Helper()
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func sign(t testing.TB, raw []byte, privHex string) string {
	t.Helper()
	sig, err := concrnt.SignBytes(raw, privHex)
	if err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(sig)
}

// Sign marshals doc (any concrnt.Document[T]) and attaches an
// ecrecover-direct proof signed by id. The document's author is whatever doc
// says; a mismatch is a valid way to build a wrong-signer fixture.
func (id Identity) Sign(t testing.TB, doc any) concrnt.SignedDocument {
	t.Helper()
	return id.SignRaw(t, marshal(t, doc))
}

// SignRaw signs already-marshalled document JSON with an ecrecover-direct
// proof.
func (id Identity) SignRaw(t testing.TB, raw []byte) concrnt.SignedDocument {
	t.Helper()
	signature := sign(t, raw, id.PrivKey)
	return concrnt.SignedDocument{
		Document: string(raw),
		Proof:    concrnt.Proof{Type: concrnt.ProofTypeEcrecover, Signature: &signature},
	}
}

// Resign re-signs sd.Document with id, keeping CCFS and CCKV. For fixtures
// whose document was edited after construction.
func Resign(t testing.TB, sd concrnt.SignedDocument, id Identity) concrnt.SignedDocument {
	t.Helper()
	out := id.SignRaw(t, []byte(sd.Document))
	out.CCFS = sd.CCFS
	out.CCKV = sd.CCKV
	return out
}

// NoneProof marshals doc with an unsigned (none) proof, the shape of a
// server-imported v1 document.
func NoneProof(t testing.TB, doc any) concrnt.SignedDocument {
	t.Helper()
	return concrnt.SignedDocument{Document: string(marshal(t, doc)), Proof: concrnt.Proof{Type: concrnt.ProofTypeNone}}
}

// Tamper replaces old with new in sd.Document, leaving the proof as it was,
// so the signature no longer matches. It fails the test if nothing changed.
func Tamper(t testing.TB, sd concrnt.SignedDocument, old, new string) concrnt.SignedDocument {
	t.Helper()
	replaced := strings.Replace(sd.Document, old, new, 1)
	if replaced == sd.Document {
		t.Fatalf("tamper: %q not found in document %s", old, sd.Document)
	}
	sd.Document = replaced
	return sd
}

// Subkey is a CIP-13 subkey: the owner's master identity, the subkey pair,
// the key URI the enact document lives at and the owner-signed enact
// document itself (what a resolver must return for URI).
type Subkey struct {
	Owner Identity
	Key   Identity
	URI   string
	Enact concrnt.SignedDocument
}

// NewSubkey enacts a fresh subkey for owner at uri, valid from enactedAt.
func NewSubkey(t testing.TB, owner Identity, uri string, enactedAt time.Time) Subkey {
	t.Helper()
	key := NewIdentity(t)
	enact := owner.Sign(t, concrnt.Document[schemas.Subkey]{
		Kind:      "record",
		Key:       uri,
		Value:     schemas.Subkey{CKID: key.CCID},
		Author:    owner.CCID,
		Schema:    schemas.SubkeyURL,
		CreatedAt: enactedAt,
	})
	return Subkey{Owner: owner, Key: key, URI: uri, Enact: enact}
}

// Sign marshals doc and attaches an ecrecover-subkey proof pointing at s.URI,
// signed with the subkey.
func (s Subkey) Sign(t testing.TB, doc any) concrnt.SignedDocument {
	t.Helper()
	raw := marshal(t, doc)
	signature := sign(t, raw, s.Key.PrivKey)
	uri := s.URI
	return concrnt.SignedDocument{
		Document: string(raw),
		Proof:    concrnt.Proof{Type: concrnt.ProofTypeSubkey, Signature: &signature, Key: &uri},
	}
}

// EntityRecord is id's owner-signed entity document placing it on domain,
// what a resolver returns for cckv://<ccid> when the client looks up the
// entity's home server.
func EntityRecord(t testing.TB, id Identity, domain string) concrnt.SignedDocument {
	t.Helper()
	return id.Sign(t, concrnt.Document[schemas.Entity]{
		Kind:      "entity",
		Key:       "cckv://" + id.CCID,
		Value:     schemas.Entity{Domain: domain},
		Author:    id.CCID,
		Schema:    schemas.EntityURL,
		CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	})
}

// Acked derives the acked/unacked document a server ships for an ack/unack
// (CIP-10 §5.2): the same fields with the kind replaced, carrying original
// in a document-direct proof.
func Acked(t testing.TB, original concrnt.SignedDocument) concrnt.SignedDocument {
	t.Helper()
	derived, err := original.DeriveAcked()
	if err != nil {
		t.Fatal(err)
	}
	return derived
}
