package caisse

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/FacileStudio/tronc/errors"
)

func (r CheckoutRequest) validate() error {
	if err := validateScope(r); err != nil {
		return err
	}
	if err := validateRedirect("SuccessURL", r.SuccessURL); err != nil {
		return err
	}
	if err := validateRedirect("CancelURL", r.CancelURL); err != nil {
		return err
	}
	if err := validateMode(r.Mode); err != nil {
		return err
	}

	adHoc, err := validateLines(r.Lines, resolveMode(r.Mode))
	if err != nil {
		return err
	}
	if adHoc && len(strings.TrimSpace(r.Currency)) != 3 {
		return errors.Invalid("caisse: Currency must be a three-letter ISO 4217 code")
	}

	return validateMetadata(r.Metadata)
}

// validateScope checks the request-level fields: the reference, the line
// count and the customer identity.
func validateScope(r CheckoutRequest) error {
	switch {
	case strings.TrimSpace(r.Reference) == "":
		return errors.Invalid("caisse: Reference is required")
	case len(r.Reference) > maxReference:
		return errors.Invalid(fmt.Sprintf("caisse: Reference is longer than %d characters", maxReference))
	case len(r.Lines) == 0:
		return errors.Invalid("caisse: at least one line is required")
	case len(r.Lines) > maxLines:
		return errors.Invalid(fmt.Sprintf("caisse: more than %d lines", maxLines))
	case r.CustomerID != "" && r.CustomerEmail != "":
		return errors.Invalid("caisse: set CustomerID or CustomerEmail, not both")
	}
	return nil
}

func resolveMode(mode Mode) Mode {
	if mode == "" {
		return ModePayment
	}
	return mode
}

func validateMode(mode Mode) error {
	switch mode {
	case "", ModePayment, ModeSubscription:
		return nil
	}
	return errors.Invalid(fmt.Sprintf("caisse: unknown mode %q", string(mode)))
}

// validateLines runs the per-line checks and reports whether any line is
// ad-hoc, which decides whether a currency is required.
func validateLines(lines []Line, mode Mode) (bool, error) {
	adHoc := false
	for index, line := range lines {
		if err := validateLine(line, mode, index); err != nil {
			return false, err
		}
		if line.adHoc() {
			adHoc = true
		}
	}
	return adHoc, nil
}

func validateLine(line Line, mode Mode, index int) error {
	where := fmt.Sprintf("caisse: line %d", index)
	switch {
	case line.PriceID != "" && (line.Label != "" || line.Amount != 0):
		return errors.Invalid(where + " sets both PriceID and an ad-hoc price")
	case line.Quantity < 0:
		return errors.Invalid(where + " has a negative quantity")
	}
	if !line.adHoc() {
		return nil
	}
	switch {
	case mode == ModeSubscription:
		return errors.Invalid(where + " has no PriceID, which subscription mode requires")
	case strings.TrimSpace(line.Label) == "":
		return errors.Invalid(where + " has no Label")
	case line.Amount <= 0:
		return errors.Invalid(where + " has a zero or negative Amount")
	}
	return nil
}

// validateRedirect refuses anything that is not an absolute http(s) URL. A
// relative or scheme-less redirect is a configuration mistake that only shows
// up once a customer has already paid.
func validateRedirect(field, raw string) error {
	if strings.TrimSpace(raw) == "" {
		return errors.Invalid("caisse: " + field + " is required")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return errors.Invalid("caisse: " + field + " is not a valid URL")
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return errors.Invalid("caisse: " + field + " must be an absolute http or https URL")
	}
	return nil
}

func validateMetadata(metadata map[string]string) error {
	if len(metadata) > maxMetadataEntries {
		return errors.Invalid(fmt.Sprintf("caisse: more than %d metadata entries", maxMetadataEntries))
	}
	for key, value := range metadata {
		switch {
		case key == ReferenceKey:
			return errors.Invalid("caisse: metadata key " + ReferenceKey + " is reserved")
		case len(key) > maxMetadataKey:
			return errors.Invalid(fmt.Sprintf("caisse: metadata key %q is longer than %d characters", key, maxMetadataKey))
		case len(value) > maxMetadataValue:
			return errors.Invalid(fmt.Sprintf("caisse: metadata value for %q is longer than %d characters", key, maxMetadataValue))
		}
	}
	return nil
}
