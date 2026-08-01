package handler

import (
	"strconv"

	pkgerr "github.com/lynn901/mora/internal/pkg/errors"
)

func parseInt(s string) (int, error) { return strconv.Atoi(s) }

func badRequestErr(msg string) error { return pkgerr.BadRequest(msg) }
