package statistics

import (
	"context"
	"database/sql"
)

type Queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}
