package db

import (
	"database/sql"
	"errors"
	"strings"
)

func isNoRowsErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, sql.ErrNoRows) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "no rows in result set")
}
