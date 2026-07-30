package handler

import (
	"strconv"

	pkgerr "github.com/wiki/wiki-backend/internal/pkg/errors"
)

func parseInt(s string) (int, error) { return strconv.Atoi(s) }

func badRequestErr(msg string) error { return pkgerr.BadRequest(msg) }
