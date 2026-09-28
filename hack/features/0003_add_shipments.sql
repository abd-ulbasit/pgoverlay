-- The pull request's migration, applied to a branch in hack/features.tape.
CREATE TABLE shipments (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    order_id   bigint NOT NULL REFERENCES orders (id),
    carrier    text NOT NULL,
    shipped_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX shipments_order_id_idx ON shipments (order_id);

INSERT INTO shipments (order_id, carrier)
SELECT id, (ARRAY['ups','fedex','dhl'])[1 + id % 3]
FROM orders
WHERE status = 'paid'
ORDER BY id
LIMIT 500;

ANALYZE shipments;
