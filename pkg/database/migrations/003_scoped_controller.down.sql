-- Revoke restricted keys before removing the discriminator; never promote them.
DELETE FROM api_keys WHERE controller = 1;
ALTER TABLE api_keys DROP COLUMN controller;
