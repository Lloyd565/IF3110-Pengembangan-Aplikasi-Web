DROP SCHEMA IF EXISTS maintenance CASCADE;
DROP SCHEMA IF EXISTS flight_ops CASCADE;
CREATE SCHEMA flight_ops;
CREATE SCHEMA maintenance;

CREATE TABLE flight_ops.airports (
    id           serial PRIMARY KEY,
    icao_code    char(4)      NOT NULL UNIQUE CHECK (icao_code ~ '^[A-Z]{4}$'),
    iata_code    char(3)      UNIQUE CHECK (iata_code ~ '^[A-Z]{3}$'),
    name         text         NOT NULL,
    city         text         NOT NULL,
    country      char(2)      NOT NULL,
    latitude     numeric(9,6) NOT NULL CHECK (latitude BETWEEN -90 AND 90),
    longitude    numeric(9,6) NOT NULL CHECK (longitude BETWEEN -180 AND 180),
    elevation_ft int,
    timezone     text         NOT NULL
);

CREATE TABLE flight_ops.aircraft (
    id                    serial PRIMARY KEY,
    registration          text NOT NULL UNIQUE CHECK (registration ~ '^[A-Z0-9]{1,2}-[A-Z0-9]{3,5}$'),
    type_code             text NOT NULL,
    manufacturer          text NOT NULL,
    model                 text NOT NULL,
    seat_capacity         int  NOT NULL CHECK (seat_capacity > 0),
    max_takeoff_weight_kg int  CHECK (max_takeoff_weight_kg > 0),
    home_base_id          int  REFERENCES flight_ops.airports(id),
    in_service_since      date NOT NULL,
    retired_at            date,
    CONSTRAINT aircraft_retired_after_service CHECK (retired_at IS NULL OR retired_at >= in_service_since)
);

CREATE TABLE flight_ops.flights (
    id                  bigserial PRIMARY KEY,
    flight_number       text        NOT NULL CHECK (flight_number ~ '^[A-Z0-9]{2}[0-9]{1,4}$'),
    aircraft_id         int         REFERENCES flight_ops.aircraft(id),
    origin_id           int         NOT NULL REFERENCES flight_ops.airports(id),
    destination_id      int         NOT NULL REFERENCES flight_ops.airports(id),
    scheduled_departure timestamptz NOT NULL,
    scheduled_arrival   timestamptz NOT NULL,
    actual_departure    timestamptz,
    actual_arrival      timestamptz,
    status              text        NOT NULL DEFAULT 'scheduled'
                        CHECK (status IN ('scheduled','boarding','departed','airborne','arrived','delayed','cancelled','diverted')),
    CONSTRAINT flights_distinct_airports CHECK (origin_id <> destination_id),
    CONSTRAINT flights_arrival_after_departure CHECK (scheduled_arrival > scheduled_departure),
    UNIQUE (flight_number, scheduled_departure)
);
CREATE INDEX flights_scheduled_departure_idx ON flight_ops.flights (scheduled_departure);
CREATE INDEX flights_route_idx ON flight_ops.flights (origin_id, destination_id);
CREATE INDEX flights_aircraft_id_idx ON flight_ops.flights (aircraft_id);

CREATE TABLE flight_ops.flight_events (
    id          bigserial PRIMARY KEY,
    flight_id   bigint      NOT NULL REFERENCES flight_ops.flights(id) ON DELETE CASCADE,
    event_type  text        NOT NULL
                CHECK (event_type IN ('scheduled','gate_assigned','boarding','pushback','takeoff','landing','arrived_gate','delayed','cancelled','diverted')),
    occurred_at timestamptz NOT NULL,
    detail      jsonb       NOT NULL DEFAULT '{}',
    recorded_by text        NOT NULL DEFAULT 'system'
);

CREATE TABLE maintenance.maintenance_records (
    id             serial PRIMARY KEY,
    aircraft_id    int          NOT NULL REFERENCES flight_ops.aircraft(id),
    check_type     text         NOT NULL CHECK (check_type IN ('daily','weekly','A-check','C-check','unscheduled')),
    description    text         NOT NULL,
    performed_at   date         NOT NULL,
    next_due       date,
    hours_at_check numeric(8,1) CHECK (hours_at_check >= 0),
    technician     text         NOT NULL,
    passed         boolean      NOT NULL DEFAULT true,
    notes          text,
    CONSTRAINT maintenance_next_due_after_performed CHECK (next_due IS NULL OR next_due > performed_at)
);
CREATE INDEX maintenance_records_aircraft_idx ON maintenance.maintenance_records (aircraft_id, performed_at DESC);

INSERT INTO flight_ops.airports (icao_code, iata_code, name, city, country, latitude, longitude, elevation_ft, timezone) VALUES
    ('WIII', 'CGK', 'Soekarno-Hatta International Airport',      'Tangerang',    'ID', -6.125567, 106.655897,   34, 'Asia/Jakarta'),
    ('WICC', 'BDO', 'Husein Sastranegara Airport',               'Bandung',      'ID', -6.900630, 107.576294, 2436, 'Asia/Jakarta'),
    ('WARR', 'SUB', 'Juanda International Airport',              'Surabaya',     'ID', -7.379830, 112.786858,    9, 'Asia/Jakarta'),
    ('WADD', 'DPS', 'I Gusti Ngurah Rai International Airport',  'Denpasar',     'ID', -8.748170, 115.167172,   14, 'Asia/Makassar'),
    ('WIMM', 'KNO', 'Kualanamu International Airport',           'Deli Serdang', 'ID',  3.642222,  98.885278,   23, 'Asia/Jakarta'),
    ('WAAA', 'UPG', 'Sultan Hasanuddin International Airport',   'Makassar',     'ID', -5.061630, 119.554042,   47, 'Asia/Makassar'),
    ('WAHI', 'YIA', 'Yogyakarta International Airport',          'Kulon Progo',  'ID', -7.907459, 110.054480,   24, 'Asia/Jakarta'),
    ('WIDD', 'BTH', 'Hang Nadim International Airport',          'Batam',        'ID',  1.121028, 104.118753,  126, 'Asia/Jakarta'),
    ('WSSS', 'SIN', 'Singapore Changi Airport',                  'Singapore',    'SG',  1.350189, 103.994433,   22, 'Asia/Singapore'),
    ('WMKK', 'KUL', 'Kuala Lumpur International Airport',        'Sepang',       'MY',  2.745578, 101.709917,   69, 'Asia/Kuala_Lumpur');

INSERT INTO flight_ops.aircraft (registration, type_code, manufacturer, model, seat_capacity, max_takeoff_weight_kg, home_base_id, in_service_since, retired_at) VALUES
    ('PK-IFA', 'B738', 'Boeing', '737-800',    189,  79016, 1, '2015-03-12', NULL),
    ('PK-IFB', 'B738', 'Boeing', '737-800',    189,  79016, 1, '2016-07-01', NULL),
    ('PK-IFC', 'B738', 'Boeing', '737-800',    189,  79016, 3, '2017-11-20', NULL),
    ('PK-IFD', 'A320', 'Airbus', 'A320-200',   180,  78000, 1, '2018-02-14', NULL),
    ('PK-IFE', 'A320', 'Airbus', 'A320-200',   180,  78000, 4, '2018-09-03', NULL),
    ('PK-IFF', 'A20N', 'Airbus', 'A320neo',    186,  79000, 1, '2021-05-25', NULL),
    ('PK-IFG', 'AT76', 'ATR',    'ATR 72-600',  70,  23000, 2, '2019-04-10', NULL),
    ('PK-IFH', 'AT76', 'ATR',    'ATR 72-600',  70,  23000, 3, '2019-06-18', NULL),
    ('PK-IFI', 'A333', 'Airbus', 'A330-300',   360, 242000, 1, '2014-12-01', NULL),
    ('PK-IFJ', 'B738', 'Boeing', '737-800',    189,  79016, 5, '2020-01-30', NULL),
    ('PK-IFK', 'A20N', 'Airbus', 'A320neo',    186,  79000, 6, '2022-08-08', NULL),
    ('PK-IFZ', 'B733', 'Boeing', '737-300',    148,  62820, 1, '1998-06-15', '2019-12-31');

WITH routes (n, origin, dest, minutes) AS (VALUES
    ( 0, 'WIII', 'WADD', 110), ( 1, 'WADD', 'WIII', 105),
    ( 2, 'WIII', 'WARR',  90), ( 3, 'WARR', 'WIII',  85),
    ( 4, 'WIII', 'WIMM', 140), ( 5, 'WIMM', 'WIII', 135),
    ( 6, 'WIII', 'WSSS', 105), ( 7, 'WSSS', 'WIII', 100),
    ( 8, 'WIII', 'WAAA', 140), ( 9, 'WAAA', 'WIII', 135),
    (10, 'WARR', 'WADD',  60), (11, 'WADD', 'WARR',  60),
    (12, 'WIII', 'WAHI',  70), (13, 'WAHI', 'WIII',  70),
    (14, 'WIII', 'WMKK', 125), (15, 'WMKK', 'WIII', 120),
    (16, 'WIII', 'WIDD',  95), (17, 'WIDD', 'WIII',  95),
    (18, 'WICC', 'WADD', 105), (19, 'WADD', 'WICC', 105)
),
plan AS (
    SELECT i, r.n,
           o.id AS origin_id, d.id AS destination_id,
           timestamptz '2026-10-22 05:00+07' + i * interval '75 minutes' AS dep,
           timestamptz '2026-10-22 05:00+07' + i * interval '75 minutes' + r.minutes * interval '1 minute' AS arr,
           CASE WHEN i % 5 = 0 THEN 45 ELSE i % 7 END * interval '1 minute' AS delay,
           timestamptz '2026-10-25 06:00+07' AS t
    FROM generate_series(1, 120) AS i
    JOIN routes r ON r.n = i % 20
    JOIN flight_ops.airports o ON o.icao_code = r.origin
    JOIN flight_ops.airports d ON d.icao_code = r.dest
)
INSERT INTO flight_ops.flights
    (flight_number, aircraft_id, origin_id, destination_id, scheduled_departure, scheduled_arrival,
     actual_departure, actual_arrival, status)
SELECT
    'IF' || (100 + n),
    1 + i % 11,
    origin_id, destination_id, dep, arr,
    CASE
        WHEN arr < t AND i % 17 = 0 THEN NULL
        WHEN dep + delay <= t        THEN dep + delay
    END,
    CASE
        WHEN arr < t AND (i % 17 = 0 OR i % 23 = 0) THEN NULL
        WHEN arr + delay <= t                       THEN arr + delay
    END,
    CASE
        WHEN arr < t AND i % 17 = 0 THEN 'cancelled'
        WHEN arr < t AND i % 23 = 0 THEN 'diverted'
        WHEN arr + delay <= t        THEN 'arrived'
        WHEN dep + delay <= t        THEN 'airborne'
        WHEN i % 13 = 0              THEN 'delayed'
        ELSE 'scheduled'
    END
FROM plan
ORDER BY i;

INSERT INTO flight_ops.flight_events (flight_id, event_type, occurred_at, detail, recorded_by)
SELECT flight_id, event_type, occurred_at, detail, recorded_by
FROM (
    SELECT f.id AS flight_id, 'scheduled' AS event_type, f.scheduled_departure - interval '30 days' AS occurred_at,
           jsonb_build_object('source', 'schedule_import') AS detail, 'system' AS recorded_by
    FROM flight_ops.flights f
    UNION ALL
    SELECT f.id, 'gate_assigned', f.scheduled_departure - interval '3 hours',
           jsonb_build_object('gate', chr(65 + (f.id % 6)::int) || (1 + f.id % 20)), 'ops_control'
    FROM flight_ops.flights f
    WHERE f.status <> 'cancelled' AND f.scheduled_departure - interval '3 hours' < timestamptz '2026-10-25 06:00+07'
    UNION ALL
    SELECT f.id, 'delayed', f.scheduled_departure - interval '1 hour',
           jsonb_build_object('reason', (ARRAY['weather', 'ATC flow control', 'late inbound aircraft', 'technical'])[1 + (f.id % 4)::int],
                              'minutes', coalesce((extract(epoch FROM f.actual_departure - f.scheduled_departure) / 60)::int, 60)),
           'ops_control'
    FROM flight_ops.flights f
    WHERE f.actual_departure - f.scheduled_departure >= interval '30 minutes' OR f.status = 'delayed'
    UNION ALL
    SELECT f.id, 'cancelled', f.scheduled_departure - interval '2 hours',
           jsonb_build_object('reason', 'crew duty time exceeded'), 'ops_control'
    FROM flight_ops.flights f WHERE f.status = 'cancelled'
    UNION ALL
    SELECT f.id, 'boarding', f.actual_departure - interval '40 minutes', '{}', 'gate_agent'
    FROM flight_ops.flights f WHERE f.actual_departure IS NOT NULL
    UNION ALL
    SELECT f.id, 'pushback', f.actual_departure - interval '5 minutes', '{}', 'ground_control'
    FROM flight_ops.flights f WHERE f.actual_departure IS NOT NULL
    UNION ALL
    SELECT f.id, 'takeoff', f.actual_departure,
           jsonb_build_object('runway', CASE WHEN f.id % 2 = 0 THEN '07R' ELSE '25L' END), 'tower'
    FROM flight_ops.flights f WHERE f.actual_departure IS NOT NULL
    UNION ALL
    SELECT f.id, 'diverted', f.actual_departure + interval '60 minutes',
           jsonb_build_object('diverted_to', CASE d.icao_code WHEN 'WIII' THEN 'WIHH' WHEN 'WSSS' THEN 'WIDD' ELSE 'WIII' END,
                              'reason', 'thunderstorm at destination'), 'tower'
    FROM flight_ops.flights f JOIN flight_ops.airports d ON d.id = f.destination_id
    WHERE f.status = 'diverted'
    UNION ALL
    SELECT f.id, 'landing', f.actual_arrival - interval '8 minutes',
           jsonb_build_object('runway', CASE WHEN f.id % 2 = 0 THEN '09' ELSE '27' END), 'tower'
    FROM flight_ops.flights f WHERE f.actual_arrival IS NOT NULL
    UNION ALL
    SELECT f.id, 'arrived_gate', f.actual_arrival, '{}', 'ground_control'
    FROM flight_ops.flights f WHERE f.actual_arrival IS NOT NULL
) e
ORDER BY occurred_at, flight_id;

INSERT INTO maintenance.maintenance_records
    (aircraft_id, check_type, description, performed_at, next_due, hours_at_check, technician, passed, notes)
SELECT
    a.id, c.check_type, c.description,
    date '2026-10-20' - (c.days_ago + a.id * 3),
    date '2026-10-20' - (c.days_ago + a.id * 3) + c.interval_days,
    round(9000 + a.id * 1375.5 - c.days_ago * 8.5, 1),
    (ARRAY['R. Hidayat', 'S. Wulandari', 'D. Prasetyo', 'M. Siregar'])[1 + (a.id + c.days_ago) % 4],
    true,
    c.notes
FROM flight_ops.aircraft a
CROSS JOIN (VALUES
    ('weekly',  'Weekly walk-around and fluid levels',     2,   7, NULL),
    ('A-check', 'A-check: landing gear, brakes, lights',  40, 120, NULL),
    ('A-check', 'A-check: landing gear, brakes, lights', 160, 120, 'Brake wear 60%, within limits')
) AS c (check_type, description, days_ago, interval_days, notes)
WHERE a.retired_at IS NULL
ORDER BY a.id, c.days_ago DESC;

INSERT INTO maintenance.maintenance_records
    (aircraft_id, check_type, description, performed_at, next_due, hours_at_check, technician, passed, notes)
VALUES
    (9,  'C-check',     'C-check: structural inspection and cabin refurbishment', '2026-03-02', '2028-03-02', 41250.0, 'D. Prasetyo',  true,  NULL),
    (3,  'unscheduled', 'Bird strike inspection on left engine',                  '2026-10-12', NULL,         15770.5, 'R. Hidayat',   false, 'Fan blade nick found, engine borescope required before release'),
    (3,  'unscheduled', 'Engine borescope after bird strike',                     '2026-10-13', NULL,         15770.5, 'M. Siregar',   true,  'Blade blended within limits, aircraft released'),
    (7,  'unscheduled', 'Propeller de-icing boot replacement',                    '2026-09-28', NULL,         22140.0, 'S. Wulandari', true,  NULL),
    (12, 'C-check',     'Final C-check before retirement',                        '2019-11-20', NULL,         68420.0, 'R. Hidayat',   true,  'Aircraft retired after this check');
