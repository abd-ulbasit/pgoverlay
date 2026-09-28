-- Synthetic "prod" database for hack/features.tape: customers carry
-- (fake) personal data, orders reference them.
CREATE TABLE customers (
    id         bigint PRIMARY KEY,
    name       text NOT NULL,
    email      text NOT NULL UNIQUE,
    phone      text,
    created_at timestamptz NOT NULL
);

CREATE TABLE orders (
    id          bigint PRIMARY KEY,
    customer_id bigint NOT NULL REFERENCES customers (id),
    status      text NOT NULL,
    total_cents integer NOT NULL,
    created_at  timestamptz NOT NULL
);
CREATE INDEX orders_customer_id_idx ON orders (customer_id);

INSERT INTO customers
SELECT g,
       f || ' ' || l,
       lower(f) || '.' || lower(l) || g || '@example.com',
       '+1-202-555-' || lpad((g % 10000)::text, 4, '0'),
       timestamptz '2024-01-01' + (g % 600) * interval '1 day'
FROM generate_series(1, 50000) AS g,
     LATERAL (SELECT (ARRAY['Ava','Liam','Noah','Emma','Mia','Omar','Zara','Ivan','Yuki','Sara'])[1 + g % 10] AS f,
                     (ARRAY['Khan','Smith','Garcia','Chen','Novak','Silva','Ito','Brown','Patel','Weber'])[1 + (g / 10) % 10] AS l) AS n;

INSERT INTO orders
SELECT g,
       1 + (g::bigint * 7919) % 50000,
       (ARRAY['paid','paid','paid','pending','refunded'])[1 + g % 5],
       500 + (g * 31) % 20000,
       timestamptz '2024-01-01' + (g % 600) * interval '1 day'
FROM generate_series(1, 500000) AS g;

VACUUM ANALYZE;
