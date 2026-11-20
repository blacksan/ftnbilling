-- "WhatsApp is the same as the phone" is stored, so the box stays ticked and the number follows the phone.
-- Forward-only; never edit once released.
SET LOCAL lock_timeout = '10s';

ALTER TABLE customers ADD COLUMN wa_same_phone boolean NOT NULL DEFAULT false;
ALTER TABLE accounts ADD COLUMN wa_same_phone boolean NOT NULL DEFAULT false;
-- rows saved before the flag existed: a WhatsApp number equal to the phone was the "same" box
UPDATE customers SET wa_same_phone = true WHERE whatsapp <> '' AND whatsapp = phone;
UPDATE accounts SET wa_same_phone = true WHERE whatsapp <> '' AND whatsapp = phone;
