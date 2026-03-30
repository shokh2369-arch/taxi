package legal

import (
	"context"
	"fmt"
	"strings"
)

// RiderAgreementPromptMessage builds the text shown before the rider accepts (active user_terms + privacy).
func (s *Service) RiderAgreementPromptMessage(ctx context.Context) (string, error) {
	docs, err := s.ActiveDocuments(ctx, []string{DocUserTerms, DocPrivacyPolicy})
	if err != nil {
		return "", err
	}
	var parts []string
	for _, key := range []string{DocUserTerms, DocPrivacyPolicy} {
		if d, ok := docs[key]; ok && strings.TrimSpace(d.Content) != "" {
			parts = append(parts, strings.TrimSpace(d.Content))
		}
	}
	if len(parts) == 0 {
		return "", fmt.Errorf("no active rider legal documents")
	}
	return strings.Join(parts, "\n\n───────────\n\n") + "\n\n👇 Davom etish uchun tasdiqlang:", nil
}

// DriverAgreementPromptMessage builds driver-facing legal text (active driver oferta + privacy only).
func (s *Service) DriverAgreementPromptMessage(ctx context.Context) (string, error) {
	docs, err := s.ActiveDocuments(ctx, []string{DocDriverTerms, DocPrivacyPolicy})
	if err != nil {
		return "", err
	}
	var parts []string
	for _, key := range []string{DocDriverTerms, DocPrivacyPolicy} {
		if d, ok := docs[key]; ok && strings.TrimSpace(d.Content) != "" {
			parts = append(parts, strings.TrimSpace(d.Content))
		}
	}
	if len(parts) == 0 {
		return "", fmt.Errorf("no active driver legal documents")
	}
	return strings.Join(parts, "\n\n───────────\n\n") + "\n\n👇 Davom etish uchun tasdiqlang:", nil
}
