package pagination

import (
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/lynn901/mora/internal/pkg/errors"
)

const (
	DefaultPage     = 1
	DefaultPageSize = 20
	MaxPageSize     = 100
)

// Params holds validated pagination state.
type Params struct {
	Page     int
	PageSize int
}

// Offset returns the SQL OFFSET value.
func (p Params) Offset() int { return (p.Page - 1) * p.PageSize }

// From parses page/page_size query params with sane defaults and bounds.
func From(c *gin.Context) Params {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	if page < 1 {
		page = DefaultPage
	}
	size, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	if size < 1 {
		size = DefaultPageSize
	}
	if size > MaxPageSize {
		size = MaxPageSize
	}
	return Params{Page: page, PageSize: size}
}

// ParseUUID parses a query param as UUID, returning empty + nil error when absent.
func ParseUUID(c *gin.Context, key string) (string, error) {
	v := c.Query(key)
	if v == "" {
		return "", nil
	}
	return v, nil
}

// RequireUUID parses a required query param as UUID.
func RequireUUID(c *gin.Context, key string) (string, error) {
	v := c.Query(key)
	if v == "" {
		return "", errors.BadRequest("missing required param: " + key)
	}
	return v, nil
}
