package game_test

import (
	"database/sql"
	"strconv"
)

type sqlTx = sql.Tx

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
