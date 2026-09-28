-- Masking script for the "prod" source (pgb source set-mask prod mask.sql):
-- runs inside every new or reset branch before it is reported ready.
UPDATE customers
   SET name  = 'Customer ' || id,
       email = 'customer' || id || '@masked.invalid',
       phone = NULL;
