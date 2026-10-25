package authz

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/reqctx"
)

type Authorizer struct {
	DB *pgxpool.Pool
}

func (a *Authorizer) effectiveLevel(ctx context.Context, userID int64, connID uuid.UUID) (Level, error) {
	var edit, view bool
	err := a.DB.QueryRow(ctx, `
		SELECT COALESCE(bool_or(g.level = 'edit'), false),
		       COALESCE(bool_or(g.level = 'view'), false)
		FROM group_members m
		JOIN db_grants g       ON g.group_id = m.group_id
		JOIN users u           ON u.id = m.user_id AND u.is_active
		JOIN db_connections c  ON c.id = g.connection_id AND c.deleted_at IS NULL
		WHERE m.user_id = $1 AND g.connection_id = $2`,
		userID, connID).Scan(&edit, &view)
	if err != nil {
		return LevelNone, err
	}
	if edit {
		return LevelEdit, nil
	}
	if view {
		return LevelView, nil
	}
	return LevelNone, nil
}

func (a *Authorizer) Can(ctx context.Context, p reqctx.Principal, connID uuid.UUID, act Action) error {
	lvl, err := a.effectiveLevel(ctx, p.UserID, connID)
	if err != nil {
		return fmt.Errorf("authz: baca grant: %w", err)
	}
	if !Allows(lvl, act) {
		return ErrForbidden
	}
	return nil
}

func (a *Authorizer) VisibleConnections(ctx context.Context, p reqctx.Principal) ([]ConnAccess, error) {
	rows, err := a.DB.Query(ctx, `
		SELECT c.id, c.name,
		       CASE WHEN bool_or(g.level = 'edit') THEN 'edit' ELSE 'view' END,
		       array_agg(DISTINCT gr.name ORDER BY gr.name)
		FROM db_connections c
		JOIN db_grants g       ON g.connection_id = c.id
		JOIN group_members m   ON m.group_id = g.group_id
		JOIN groups gr         ON gr.id = g.group_id
		JOIN users u           ON u.id = m.user_id AND u.is_active
		WHERE m.user_id = $1 AND c.deleted_at IS NULL
		GROUP BY c.id, c.name
		ORDER BY c.name`, p.UserID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ConnAccess
	for rows.Next() {
		var ca ConnAccess
		var lvl string
		if err := rows.Scan(&ca.ConnectionID, &ca.Name, &lvl, &ca.Groups); err != nil {
			return nil, err
		}
		ca.Level = Level(lvl)
		out = append(out, ca)
	}
	return out, rows.Err()
}
