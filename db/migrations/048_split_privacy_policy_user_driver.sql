-- +goose Up
-- Split privacy policy into rider/user vs driver versions.
-- - Keeps legacy privacy_policy rows/acceptances for history.
-- - Introduces privacy_policy_user and privacy_policy_driver as new document types.
-- - Preserves existing data by rebuilding CHECK constraints and copying rows.
-- - Activates new policies and deactivates legacy privacy_policy.

PRAGMA foreign_keys=OFF;
BEGIN;

-- Rebuild legal_documents with expanded document_type CHECK.
DROP INDEX IF EXISTS idx_legal_documents_one_active_per_type;
ALTER TABLE legal_documents RENAME TO legal_documents_old;

CREATE TABLE legal_documents (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  document_type TEXT NOT NULL CHECK (document_type IN (
    'driver_terms','user_terms','privacy_policy',
    'privacy_policy_user','privacy_policy_driver'
  )),
  version INTEGER NOT NULL,
  content TEXT NOT NULL,
  is_active INTEGER NOT NULL DEFAULT 0 CHECK (is_active IN (0, 1)),
  created_at TEXT NOT NULL DEFAULT (datetime('now')),
  UNIQUE (document_type, version)
);

CREATE UNIQUE INDEX idx_legal_documents_one_active_per_type ON legal_documents(document_type) WHERE is_active = 1;

INSERT INTO legal_documents (id, document_type, version, content, is_active, created_at)
SELECT id, document_type, version, content, is_active, created_at
FROM legal_documents_old;

DROP TABLE legal_documents_old;

-- Rebuild legal_acceptances with expanded document_type CHECK.
ALTER TABLE legal_acceptances RENAME TO legal_acceptances_old;

CREATE TABLE legal_acceptances (
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  document_type TEXT NOT NULL CHECK (document_type IN (
    'driver_terms','user_terms','privacy_policy',
    'privacy_policy_user','privacy_policy_driver'
  )),
  version INTEGER NOT NULL,
  accepted_at TEXT NOT NULL DEFAULT (datetime('now')),
  client_ip TEXT,
  user_agent TEXT,
  PRIMARY KEY (user_id, document_type)
);

INSERT INTO legal_acceptances (user_id, document_type, version, accepted_at, client_ip, user_agent)
SELECT user_id, document_type, version, accepted_at, client_ip, user_agent
FROM legal_acceptances_old;

DROP TABLE legal_acceptances_old;

-- Activate split privacy policies and deactivate the legacy shared one.
UPDATE legal_documents
SET is_active = 0
WHERE document_type = 'privacy_policy' AND is_active = 1;

INSERT INTO legal_documents (document_type, version, content, is_active) VALUES
('privacy_policy_user', 1,
'📄 Maxfiylik siyosati

YettiQanot foydalanuvchi ma’lumotlarini xizmatni ta’minlash uchun qayta ishlaydi.

1. Yig‘iladigan ma’lumotlar:
- telefon raqam
- Telegram ID
- joylashuv (location)
- buyurtma ma’lumotlari

2. Maqsad:
- haydovchi va mijozni bog‘lash
- buyurtmalarni uzatish
- xavfsizlik

3. Ma’lumotlar sotilmaydi.

4. Platformadan foydalanish orqali siz rozilik bildirasiz.',
1),
('privacy_policy_driver', 1,
'📄 Maxfiylik siyosati (haydovchilar uchun)

YettiQanot haydovchi ma’lumotlarini xizmatni ta’minlash va haydovchini/transport vositasini identifikatsiya qilish uchun qayta ishlaydi.

1. Yig‘iladigan ma’lumotlar:
- telefon raqam
- Telegram ID
- joylashuv (location)
- buyurtma ma’lumotlari
- haydovchilik guvohnomasi ma’lumotlari
- avtotransport vositasi ro‘yxatdan o‘tganligi to‘g‘risidagi guvohnoma (tex pasport) ma’lumotlari

2. Maqsad:
- haydovchi va mijozni bog‘lash
- buyurtmalarni uzatish
- xavfsizlik
- haydovchini va transport vositasini identifikatsiya qilish / tekshirish

3. Ma’lumotlar sotilmaydi.

4. Platformadan foydalanish orqali siz rozilik bildirasiz.',
1);

-- Force re-accept for affected actors via the new document_type(s).
UPDATE users SET terms_accepted = 0 WHERE role = 'rider';
UPDATE drivers SET terms_accepted = 0;

COMMIT;
PRAGMA foreign_keys=ON;

-- +goose Down
-- Revert by removing new policy types and restoring legacy shared privacy_policy as active.
PRAGMA foreign_keys=OFF;
BEGIN;

DELETE FROM legal_acceptances WHERE document_type IN ('privacy_policy_user','privacy_policy_driver');
DELETE FROM legal_documents WHERE document_type IN ('privacy_policy_user','privacy_policy_driver');

UPDATE legal_documents
SET is_active = 1
WHERE document_type = 'privacy_policy'
  AND version = (SELECT MAX(version) FROM legal_documents WHERE document_type = 'privacy_policy');

COMMIT;
PRAGMA foreign_keys=ON;
