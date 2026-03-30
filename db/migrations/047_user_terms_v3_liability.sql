-- +goose Up
-- Bump user terms to v3 and add platform liability disclaimer; requires re-acceptance via active version checks.
UPDATE legal_documents SET is_active = 0 WHERE document_type = 'user_terms' AND is_active = 1;

INSERT INTO legal_documents (document_type, version, content, is_active) VALUES
('user_terms', 3,
'📄 Foydalanuvchi shartlari

YettiQanot — foydalanuvchilarni haydovchilar bilan bog‘lovchi platformadir.

1. Platforma transport xizmatini bevosita ko‘rsatmaydi.

2. YettiQanot MChJ faqat haydovchi va mijozni bog‘lovchi platforma hisoblanadi.
Platforma transport xizmatlarini ko‘rsatmaydi va haydovchi tomonidan ko‘rsatiladigan xizmatlar sifati, xavfsizligi yoki natijalari uchun javobgar emas.

3. Foydalanuvchi to‘g‘ri ma’lumot kiritishi shart.

4. Joylashuv (location) buyurtma uchun ishlatiladi.

5. To‘lovlar haydovchi bilan to‘g‘ridan-to‘g‘ri amalga oshiriladi.

6. Noto‘g‘ri foydalanish hisob bloklanishiga olib kelishi mumkin.

7. Platforma qoidalari yangilanishi mumkin.

8. Davom etish orqali siz ushbu shartlarga rozilik bildirasiz.',
1);

-- +goose Down
DELETE FROM legal_documents WHERE document_type = 'user_terms' AND version = 3;
UPDATE legal_documents SET is_active = 1 WHERE document_type = 'user_terms' AND version = 2;

