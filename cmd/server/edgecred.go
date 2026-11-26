package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/israel-duff/xenibase/internal/config"
	"github.com/israel-duff/xenibase/internal/crypto"
	"github.com/israel-duff/xenibase/internal/edgecred"
	"github.com/israel-duff/xenibase/internal/store"
)

// edgeCredential implements -issue-edge-credential and
// -revoke-edge-credential: an edge's own credential, bound to one region,
// for its XENIBASE_EDGE_SECRET (docs/backend-services.md).
func edgeCredential(cfg config.Config, keyring *crypto.Keyring, log *slog.Logger, issue, region, revoke string) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool, err := connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := store.Migrate(ctx, pool, log); err != nil {
		return err
	}
	if revoke != "" {
		if err := edgecred.Revoke(ctx, pool, revoke); err != nil {
			return err
		}
		log.Info("edge credential revoked", "edge", revoke)
		return nil
	}
	if region == "" {
		return errors.New("-issue-edge-credential needs -edge-region, the region the edge serves")
	}
	cred, err := edgecred.Issue(ctx, pool, keyring, issue, region, nil)
	if err != nil {
		return err
	}
	fmt.Println(cred)
	return nil
}
