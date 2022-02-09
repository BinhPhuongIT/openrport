package schedule

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/jmoiron/sqlx"

	"github.com/cloudradar-monitoring/rport/share/query"
)

type SQLiteProvider struct {
	db *sqlx.DB
}

func newSQLiteProvider(db *sqlx.DB) *SQLiteProvider {
	return &SQLiteProvider{
		db: db,
	}
}

func (p *SQLiteProvider) Insert(ctx context.Context, s *Schedule) error {
	_, err := p.db.NamedExecContext(ctx,
		`INSERT INTO schedules (
			id,
			created_at,
			created_by,
			name,
			schedule,
			type,
			details
		) VALUES (
			:id,
			:created_at,
			:created_by,
			:name,
			:schedule,
			:type,
			:details
		)`,
		s.ToDB(),
	)

	return err
}

func (p *SQLiteProvider) Update(ctx context.Context, s *Schedule) error {
	_, err := p.db.NamedExecContext(ctx,
		`UPDATE schedules SET
			name = :name,
			schedule = :schedule,
			type = :type,
			details = :details
		WHERE id = :id`,
		s.ToDB(),
	)

	return err
}

func (p *SQLiteProvider) List(ctx context.Context, options *query.ListOptions) ([]*Schedule, error) {
	values := []*DBSchedule{}

	q := "SELECT * FROM `schedules`"

	q, params := query.ConvertListOptionsToQuery(options, q)

	err := p.db.SelectContext(ctx, &values, q, params...)
	if err != nil {
		return nil, err
	}

	result := make([]*Schedule, len(values))
	for i, v := range values {
		result[i] = v.ToSchedule()
	}

	return result, nil
}

func (p *SQLiteProvider) Close() error {
	return p.db.Close()
}

func (p *SQLiteProvider) Get(ctx context.Context, id string) (*Schedule, error) {
	q := "SELECT * FROM `schedules` WHERE `id` = ? LIMIT 1"

	s := &DBSchedule{}
	err := p.db.GetContext(ctx, s, q, id)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}

	return s.ToSchedule(), nil
}

func (p *SQLiteProvider) Delete(ctx context.Context, id string) error {
	res, err := p.db.ExecContext(ctx, "DELETE FROM `schedules` WHERE `id` = ?", id)

	if err != nil {
		return err
	}

	affectedRows, err := res.RowsAffected()
	if err != nil {
		return err
	}

	if affectedRows == 0 {
		return fmt.Errorf("cannot find entry by id %s", id)
	}

	return nil
}
