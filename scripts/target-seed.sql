DROP SCHEMA IF EXISTS shop CASCADE;
DROP SCHEMA IF EXISTS hr CASCADE;
CREATE SCHEMA shop;
CREATE SCHEMA hr;

CREATE TABLE shop.customers (
    id         bigserial PRIMARY KEY,
    email      text        NOT NULL UNIQUE,
    full_name  text        NOT NULL,
    city       text,
    is_member  boolean     NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX customers_city_idx ON shop.customers (city);

CREATE TABLE shop.products (
    id         bigserial PRIMARY KEY,
    sku        text          NOT NULL UNIQUE,
    name       text          NOT NULL,
    price      numeric(12,2) NOT NULL CHECK (price >= 0),
    stock      int           NOT NULL DEFAULT 0 CHECK (stock >= 0),
    attributes jsonb         NOT NULL DEFAULT '{}'
);

CREATE TABLE shop.orders (
    id          bigserial PRIMARY KEY,
    customer_id bigint      NOT NULL REFERENCES shop.customers(id),
    status      text        NOT NULL DEFAULT 'pending'
                CHECK (status IN ('pending','paid','shipped','cancelled')),
    ordered_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX orders_customer_id_idx ON shop.orders (customer_id);

CREATE TABLE shop.order_items (
    order_id   bigint        NOT NULL REFERENCES shop.orders(id) ON DELETE CASCADE,
    product_id bigint        NOT NULL REFERENCES shop.products(id),
    qty        int           NOT NULL CHECK (qty > 0),
    unit_price numeric(12,2) NOT NULL,
    PRIMARY KEY (order_id, product_id)
);

INSERT INTO shop.customers (email, full_name, city, is_member, created_at)
SELECT 'customer' || i || '@example.com',
       'Customer ' || i,
       (ARRAY['Bandung','Jakarta','Surabaya','Medan', NULL])[1 + i % 5],
       i % 3 = 0,
       timestamptz '2026-01-01' + i * interval '1 day'
FROM generate_series(1, 150) AS i;

INSERT INTO shop.products (sku, name, price, stock, attributes) VALUES
    ('BK-001', 'Buku Tulis A5',    12500.00, 200, '{"color": "blue"}'),
    ('BK-002', 'Buku Gambar A4',   18000.00,  80, '{"color": "white"}'),
    ('PN-001', 'Pulpen Hitam',      4500.00, 500, '{}'),
    ('PN-002', 'Pensil 2B',         3000.00, 350, '{"pack": 12}'),
    ('TS-001', 'Tas Ransel',      245000.00,  15, '{"size": "L"}'),
    ('KL-001', 'Kalkulator Ilmiah',189000.00,  0, '{"brand": "generic"}');

INSERT INTO shop.orders (customer_id, status, ordered_at)
SELECT 1 + (i * 7) % 150,
       (ARRAY['pending','paid','shipped','cancelled'])[1 + i % 4],
       timestamptz '2026-06-01' + i * interval '6 hours'
FROM generate_series(1, 60) AS i;

INSERT INTO shop.order_items (order_id, product_id, qty, unit_price)
SELECT o.id, p.id, 1 + (o.id + p.id) % 3, p.price
FROM shop.orders o
JOIN shop.products p ON (o.id + p.id) % 3 = 0;

CREATE TABLE hr.departments (
    id   serial PRIMARY KEY,
    code text NOT NULL UNIQUE,
    name text NOT NULL
);

CREATE TABLE hr.employees (
    id            serial PRIMARY KEY,
    department_id int           REFERENCES hr.departments(id) ON DELETE SET NULL,
    manager_id    int           REFERENCES hr.employees(id),
    full_name     text          NOT NULL,
    hired_on      date          NOT NULL,
    salary        numeric(12,2) CHECK (salary > 0)
);
CREATE INDEX employees_department_id_idx ON hr.employees (department_id);

INSERT INTO hr.departments (code, name) VALUES
    ('ENG', 'Engineering'),
    ('OPS', 'Operations'),
    ('FIN', 'Finance');

INSERT INTO hr.employees (department_id, manager_id, full_name, hired_on, salary) VALUES
    (1, NULL, 'Ani Wijaya',    '2020-02-01', 25000000),
    (1, 1,    'Budi Santoso',  '2021-07-15', 15000000),
    (1, 1,    'Citra Lestari', '2022-03-10', 14000000),
    (2, NULL, 'Dedi Kurniawan','2019-11-20', 22000000),
    (2, 4,    'Eka Putri',     '2023-01-05', 11000000),
    (3, NULL, 'Fajar Nugroho', '2018-05-30', 23000000),
    (NULL, NULL, 'Gita Maharani','2024-08-01', NULL);
