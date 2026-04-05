package legal

import (
	"context"
	"fmt"
	"strings"
)

// RiderAgreementPromptMessage builds the text shown before the rider accepts (active user_terms + privacy).
func (s *Service) RiderAgreementPromptMessage(ctx context.Context) (string, error) {
	docs, err := s.ActiveDocuments(ctx, []string{DocUserTerms, DocPrivacyPolicyUser})
	if err != nil {
		return "", err
	}
	var parts []string
	for _, key := range []string{DocUserTerms, DocPrivacyPolicyUser} {
		if d, ok := docs[key]; ok && strings.TrimSpace(d.Content) != "" {
			parts = append(parts, strings.TrimSpace(d.Content))
		}
	}
	if len(parts) == 0 {
		return "", fmt.Errorf("no active rider legal documents")
	}
	return strings.Join(parts, "\n\n───────────\n\n") + "\n\n👇 Давом этиш учун тасдиқланг:", nil
}

// DriverAgreementPromptMessage builds driver-facing legal text (active driver oferta + privacy only).
func (s *Service) DriverAgreementPromptMessage(ctx context.Context) (string, error) {
	docs, err := s.ActiveDocuments(ctx, []string{DocDriverTerms, DocPrivacyPolicyDriver})
	if err != nil {
		return "", err
	}
	var parts []string
	for _, key := range []string{DocDriverTerms, DocPrivacyPolicyDriver} {
		if d, ok := docs[key]; ok && strings.TrimSpace(d.Content) != "" {
			parts = append(parts, strings.TrimSpace(d.Content))
		}
	}
	if len(parts) == 0 {
		return "", fmt.Errorf("no active driver legal documents")
	}
	return strings.Join(parts, "\n\n───────────\n\n") + "\n\n👇 Давом этиш учун тасдиқланг:", nil
}
