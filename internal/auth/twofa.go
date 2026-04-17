package auth

import (
	"context"
	"fmt"
	"log"
	"time"

	"dexFiatController/internal/connector"
)

type Resolver struct {
	totp    *TOTPClient
	passkey *PasskeyManager
	sms     SMSProvider
	email   EmailProvider
}

func NewResolver(totp *TOTPClient, passkey *PasskeyManager, sms SMSProvider, email EmailProvider) *Resolver {
	return &Resolver{
		totp:    totp,
		passkey: passkey,
		sms:     sms,
		email:   email,
	}
}

func (r *Resolver) ResolveChallenge(ctx context.Context, service string, pref connector.TwoFAPreference) (string, error) {
	methods := []connector.TwoFAMethod{pref.Primary, pref.Secondary, pref.Fallback}

	for _, method := range methods {
		if method == connector.TwoFANone {
			continue
		}

		code, err := r.tryMethod(ctx, service, method)
		if err != nil {
			log.Printf("[auth] %s: method %s failed: %v", service, method, err)
			continue
		}
		if code != "" {
			log.Printf("[auth] %s: resolved 2FA via %s", service, method)
			return code, nil
		}
	}

	return "", fmt.Errorf("auth: all 2FA methods exhausted for %s", service)
}

func (r *Resolver) tryMethod(ctx context.Context, service string, method connector.TwoFAMethod) (string, error) {
	switch method {
	case connector.TwoFATOTP:
		return r.handleTOTP(ctx, service)
	case connector.TwoFAPasskey:
		return r.handlePasskey(ctx, service)
	case connector.TwoFASMS:
		return r.handleSMS(ctx, service)
	case connector.TwoFAEmail:
		return r.handleEmail(ctx, service)
	case connector.TwoFACall:
		return r.handleSMS(ctx, service)
	default:
		return "", fmt.Errorf("unsupported 2FA method: %s", method)
	}
}

func (r *Resolver) handleTOTP(ctx context.Context, service string) (string, error) {
	if r.totp == nil {
		return "", fmt.Errorf("no TOTP client configured")
	}

	code, _, err := r.totp.GenerateCode(ctx, service)
	if err != nil {
		return "", fmt.Errorf("totp generate: %w", err)
	}
	return code, nil
}

func (r *Resolver) handlePasskey(ctx context.Context, service string) (string, error) {
	if r.passkey == nil {
		return "", fmt.Errorf("no passkey manager configured")
	}

	if !r.passkey.IsEnabled(ctx) {
		if err := r.passkey.Enable(ctx); err != nil {
			return "", fmt.Errorf("passkey enable: %w", err)
		}
	}

	if err := r.passkey.SetUserVerified(ctx, true); err != nil {
		return "", fmt.Errorf("passkey set user verified: %w", err)
	}

	log.Printf("[auth] passkey authenticator ready for %s - browser will auto-respond to WebAuthn challenge", service)
	return "passkey:authenticated", nil
}

func (r *Resolver) handleSMS(ctx context.Context, service string) (string, error) {
	if r.sms == nil {
		return "", fmt.Errorf("no SMS provider configured")
	}
	code, err := r.sms.WaitForCode(ctx, "", 120*time.Second)
	if err != nil {
		return "", err
	}
	return code, nil
}

func (r *Resolver) handleEmail(ctx context.Context, service string) (string, error) {
	if r.email == nil {
		return "", fmt.Errorf("no email provider configured")
	}
	code, err := r.email.WaitForCode(ctx, "", 120*time.Second)
	if err != nil {
		return "", err
	}
	return code, nil
}

func TwoFAPreferences(service string) connector.TwoFAPreference {
	switch service {
	case "paypal":
		return connector.TwoFAPreference{
			Primary:   connector.TwoFAPasskey,
			Secondary: connector.TwoFATOTP,
		}
	case "wise":
		return connector.TwoFAPreference{
			Primary:   connector.TwoFAPasskey,
			Secondary: connector.TwoFATOTP,
			Fallback:  connector.TwoFASMS,
		}
	case "revolut":
		return connector.TwoFAPreference{
			Primary:   connector.TwoFAPasskey,
			Secondary: connector.TwoFASMS,
			Fallback:  connector.TwoFAEmail,
		}
	default:
		return connector.TwoFAPreference{
			Primary: connector.TwoFAPasskey,
		}
	}
}
