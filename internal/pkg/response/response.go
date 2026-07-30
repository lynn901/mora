package response

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/wiki/wiki-backend/internal/pkg/errors"
)

type Envelope struct {
	Code    errors.Code `json:"code"`
	Data    any         `json:"data"`
	Message string      `json:"message"`
}

type Page struct {
	Items    any `json:"items"`
	Total    int `json:"total"`
	Page     int `json:"page"`
	PageSize int `json:"page_size"`
}

func OK(c *gin.Context, data any) {
	c.JSON(http.StatusOK, Envelope{Code: errors.CodeOK, Data: data, Message: "ok"})
}

func Created(c *gin.Context, data any) {
	c.JSON(http.StatusCreated, Envelope{Code: errors.CodeOK, Data: data, Message: "created"})
}

func Accepted(c *gin.Context, data any) {
	c.JSON(http.StatusAccepted, Envelope{Code: errors.CodeOK, Data: data, Message: "accepted"})
}

func NoContent(c *gin.Context) {
	c.Status(http.StatusNoContent)
}

func Paged(c *gin.Context, items any, total, page, pageSize int) {
	OK(c, Page{Items: items, Total: total, Page: page, PageSize: pageSize})
}

// Fail writes an error envelope. If err is an *errors.Error its code/status
// are honored; otherwise it is treated as an internal error.
func Fail(c *gin.Context, err error) {
	if err == nil {
		c.Status(http.StatusNoContent)
		return
	}
	if e := errors.As(err); e != nil {
		c.JSON(e.Status, Envelope{Code: e.Code, Data: nil, Message: e.Msg})
		return
	}
	c.JSON(http.StatusInternalServerError, Envelope{
		Code:    errors.CodeInternal,
		Data:    nil,
		Message: "internal server error",
	})
}
