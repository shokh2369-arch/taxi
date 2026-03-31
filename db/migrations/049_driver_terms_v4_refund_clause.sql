-- +goose Up
-- Bump driver legal offer (driver_terms) to v4 with refund/balance clause.
-- User terms and other legal documents remain unchanged.

UPDATE legal_documents SET is_active = 0 WHERE document_type = 'driver_terms' AND is_active = 1;

INSERT INTO legal_documents (document_type, version, content, is_active) VALUES
('driver_terms', 4,
'📄 Haydovchi shartnomasi (oferta)

YettiQanot — haydovchi va mijozni bog‘lovchi platforma bo‘lib, transport xizmatini bevosita ko‘rsatmaydi.

1. Haydovchi mustaqil faoliyat yuritadi va YettiQanot xodimi hisoblanmaydi.

2. Haydovchi quyidagilar uchun to‘liq javobgar:
- transport vositasi holati
- haydovchilik guvohnomasi
- yo‘l harakati qoidalariga rioya qilish

3. YettiQanot faqat buyurtmalarni uzatadi va safar jarayoni uchun javobgar emas.

4. To‘lovlar mijoz va haydovchi o‘rtasida amalga oshiriladi. YettiQanot hozirda to‘lovlarni qabul qilmaydi.

5. Platforma 5% komissiya qo‘llashi mumkin.
Komissiya platforma qoidalariga muvofiq ichki hisob-kitoblar orqali aks ettiriladi.

6. Platforma haydovchilarga promo kredit (bonus balans) berishi mumkin:
- bu real pul emas
- naqdlashtirilmaydi
- bank hisobiga chiqarilmaydi
- faqat platforma ichida ishlatiladi

7. Platforma qoidalariga zid harakatlar aniqlansa, hisob bloklanishi mumkin.

8. Platforma qoidalari kelgusida yangilanishi mumkin.

9. Balans va qaytarish: Haydovchi tomonidan platformaga kiritilgan real mablag‘ (cash balance) haydovchi arizasiga asosan, platforma belgilagan tartib va muddatlarda qaytarilishi mumkin. Promo kredit (bonuslar) real pul hisoblanmaydi, naqdlashtirilmaydi va hech qanday holatda qaytarilmaydi. Platforma ichida hisoblangan komissiyalar va bajarilgan xizmatlar uchun ushlab qolingan mablag‘lar qaytarilmaydi. YettiQanot MChJ faqat platforma ichidagi balanslar bo‘yicha javobgar bo‘lib, haydovchi va mijoz o‘rtasidagi to‘lovlar bo‘yicha javobgar emas.',
1);

-- +goose Down
DELETE FROM legal_documents WHERE document_type = 'driver_terms' AND version = 4;
UPDATE legal_documents SET is_active = 1 WHERE document_type = 'driver_terms' AND version = 3;

