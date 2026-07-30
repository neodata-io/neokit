package fiberx

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/go-playground/validator/v10"
	"github.com/gofiber/fiber/v3"
)

// BindAndValidate binds the request body into req and validates it. On failure it
// returns an *APIError (400 + public message + machine code, and a per-field
// `fields` array for validation) for the app's ErrorHandler to render; it does
// NOT write the response itself. Writing here and returning the JSON call's nil
// error silently defeated the caller's `if err != nil` guard, so the handler ran
// on invalid/zero-value input and overwrote the 400.
func BindAndValidate(c fiber.Ctx, v *validator.Validate, req any) error {
	if err := c.Bind().Body(req); err != nil {
		return &APIError{Status: fiber.StatusBadRequest, Code: "invalid_body", Message: "invalid request body"}
	}
	if err := v.Struct(req); err != nil {
		return validationError(err)
	}
	return nil
}

// NewValidator builds a validator whose field errors use json tag names.
func NewValidator() *validator.Validate {
	v := validator.New()
	v.RegisterTagNameFunc(func(fld reflect.StructField) string {
		name := strings.SplitN(fld.Tag.Get("json"), ",", 2)[0]
		if name == "-" {
			return ""
		}
		return name
	})
	return v
}

// validationError converts go-playground/validator errors into an *APIError:
// `error` stays the joined human sentence (using json field names set via
// RegisterTagNameFunc), `code` is the stable "validation_failed", and `fields`
// carries one machine-readable entry per failing field so a client can attach the
// message to the right input instead of parsing the sentence.
func validationError(err error) *APIError {
	errs, ok := err.(validator.ValidationErrors)
	if !ok {
		return &APIError{Status: fiber.StatusBadRequest, Code: "validation_failed", Message: err.Error()}
	}
	fields := make([]FieldError, 0, len(errs))
	msgs := make([]string, 0, len(errs))
	for _, e := range errs {
		human := humanizeTag(e.Tag())
		fields = append(fields, FieldError{Field: e.Field(), Code: e.Tag(), Message: human})
		msgs = append(msgs, fmt.Sprintf("%s: %s", e.Field(), human))
	}
	return &APIError{
		Status:  fiber.StatusBadRequest,
		Code:    "validation_failed",
		Message: strings.Join(msgs, ", "),
		Fields:  fields,
	}
}

func humanizeTag(tag string) string {
	switch tag {
	case "required":
		return "required"
	case "email":
		return "must be a valid email address"
	case "min":
		return "value is too short"
	case "max":
		return "value is too long"
	case "oneof":
		return "must be one of the allowed values"
	default:
		return "invalid value"
	}
}
