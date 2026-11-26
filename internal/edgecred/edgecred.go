// Package edgecred issues and opens xenibase-edge's own credentials: one
// per edge, bound to one region, so a compromised edge exposes only its
// region's projects. The server signs nothing with them; it checks the
// edge's signatures, so it keeps each credential sealed with the master key
// (associated data "edge_credentials.secret_enc:<id>") rather than hashed.
package edgecred

import (
	"context"
	"errors"
	"fmt"
	"regexp"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/israel-duff/xenibase/internal/crypto"
	"github.com/israel-duff/xenibase/internal/edgeapi"
	"github.com/israel-duff/xenibase/internal/store"
)

// Errors.
var (
	ErrInvalid  = errors.New("invalid edge credential request")
	ErrConflict = errors.New("the edge already has a credential")
	ErrNotFound = errors.New("no such edge credential")
)

func aad(id uuid.UUID) []byte { return []byte("edge_credentials.secret_enc:" + id.String()) }

var nameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

// Issue returns the credential for the edge called name, bound to region:
// its live one if it has one for that region (so a retried edge node
// creation gets the same), otherwise a new one.
func Issue(ctx context.Context, db *pgxpool.Pool, k *crypto.Keyring, name, region string, by *uuid.UUID) (string, error) {
	if !nameRe.MatchString(name) {
		return "", fmt.Errorf("%w: an edge's name is letters, digits, '.', '_' and '-'", ErrInvalid)
	}
	var out string
	err := pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		q := store.New(tx)
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('xenibase_edge_credential:' || $1))`, name); err != nil {
			return err
		}
		if _, err := q.GetRegion(ctx, region); errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: no region %q", ErrInvalid, region)
		} else if err != nil {
			return err
		}
		cur, err := q.GetLiveEdgeCredentialByName(ctx, name)
		switch {
		case err == nil && cur.Region == region:
			b, err := k.Decrypt(cur.SecretEnc, aad(cur.ID))
			out = string(b)
			return err
		case err == nil:
			return fmt.Errorf("%w: edge %q has one for region %s; revoke it first", ErrConflict, name, cur.Region)
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		id := uuid.New()
		cred, err := edgeapi.NewCredential(id)
		if err != nil {
			return err
		}
		sealed, err := k.Encrypt([]byte(cred), aad(id))
		if err != nil {
			return err
		}
		if _, err := q.InsertEdgeCredential(ctx, store.InsertEdgeCredentialParams{ID: id, Name: name, Region: region, SecretEnc: sealed, CreatedBy: by}); err != nil {
			return err
		}
		out = cred
		return nil
	})
	return out, err
}

// Revoke revokes the live credential of the edge called name.
func Revoke(ctx context.Context, db *pgxpool.Pool, name string) error {
	n, err := store.New(db).RevokeEdgeCredential(ctx, name)
	if err == nil && n == 0 {
		return ErrNotFound
	}
	return err
}

// Open returns a live credential and its secret; ErrNotFound when it is
// unknown or revoked.
func Open(ctx context.Context, q *store.Queries, k *crypto.Keyring, id uuid.UUID) (store.EdgeCredential, string, error) {
	c, err := q.GetLiveEdgeCredential(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, "", ErrNotFound
	}
	if err != nil {
		return c, "", err
	}
	b, err := k.Decrypt(c.SecretEnc, aad(c.ID))
	if err != nil {
		return c, "", fmt.Errorf("edge credential %s: %w", c.Name, err)
	}
	return c, string(b), nil
}
