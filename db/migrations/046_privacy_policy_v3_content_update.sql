-- +goose Up
-- Update privacy_policy v3 content to include driver document data with standardized bullet list (no version bump).
UPDATE legal_documents
SET content =
'📄 Maxfiylik siyosati

YettiQanot foydalanuvchi ma’lumotlarini xizmatni ta’minlash uchun qayta ishlaydi.

1. Yig‘iladigan ma’lumotlar:
- telefon raqam
- Telegram ID
- joylashuv (location)
- buyurtma ma’lumotlari
- haydovchilik guvohnomasi ma’lumotlari
- avtotransport vositasi ro‘yxatdan o‘tganligi to‘g‘risidagi guvohnoma (tex passport) ma’lumotlari

2. Maqsad:
- haydovchi va mijozni bog‘lash
- buyurtmalarni uzatish
- xavfsizlik
- haydovchini va transport vositasini identifikatsiya qilish hamda tekshirish

3. Ma’lumotlar sotilmaydi.

4. Platformadan foydalanish orqali siz rozilik bildirasiz.'
WHERE document_type = 'privacy_policy' AND version = 3;

-- +goose Down
-- Restore original privacy_policy v3 text (as introduced by migration 044, with shortened “bo‘yicha” wording).
UPDATE legal_documents
SET content =
'📄 Maxfiylik siyosati

YettiQanot foydalanuvchi ma’lumotlarini xizmatni ta’minlash uchun qayta ishlaydi.

1. Yig‘iladigan ma’lumotlar:
- telefon raqam
- Telegram ID
- joylashuv (location)
- buyurtma ma’lumotlari
- haydovchilik guvohnomasi bo‘yicha ma’lumotlar (jumladan, surat yoki skan-nusxa orqali taqdim etilishi mumkin)
- transport vositasi texnik pasporti bo‘yicha ma’lumotlar (jumladan, surat yoki skan-nusxa orqali taqdim etilishi mumkin)

2. Maqsad:
- haydovchi va mijozni bog‘lash
- buyurtmalarni uzatish
- xavfsizlik
- haydovchini va transport vositasini identifikatsiya qilish hamda tekshirish

3. Ma’lumotlar sotilmaydi.

4. Platformadan foydalanish orqali siz rozilik bildirasiz.'
WHERE document_type = 'privacy_policy' AND version = 3;

