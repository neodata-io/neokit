package fiberx

import (
	"net/url"
	"strings"

	"github.com/gofiber/fiber/v3"
)

// Created answers 201 with body and a Location naming the new resource, so a
// client can address what it made without building the URL itself.
func Created(c fiber.Ctx, location string, body any) error {
	c.Location(location)
	return c.Status(fiber.StatusCreated).JSON(body)
}

// MemberURL is the URL of member id of the collection this request was posted
// to: POST /x/links creating "7" is /x/links/7. The request's own path is used,
// so a collection reachable at two addresses points back at the one the client
// used.
func MemberURL(c fiber.Ctx, id string) string {
	return strings.TrimSuffix(c.Path(), "/") + "/" + url.PathEscape(id)
}
