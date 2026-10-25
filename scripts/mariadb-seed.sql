CREATE TABLE airports (
    id INT PRIMARY KEY AUTO_INCREMENT,
    code CHAR(3) NOT NULL UNIQUE,
    name VARCHAR(150) NOT NULL,
    city VARCHAR(100) NOT NULL
) ENGINE=InnoDB;

CREATE TABLE flights (
    id BIGINT PRIMARY KEY AUTO_INCREMENT,
    flight_number VARCHAR(10) NOT NULL,
    origin_id INT NOT NULL,
    destination_id INT NOT NULL,
    departure_at DATETIME NOT NULL,
    status VARCHAR(20) NOT NULL DEFAULT 'scheduled',
    CONSTRAINT flights_origin_fk FOREIGN KEY (origin_id) REFERENCES airports(id),
    CONSTRAINT flights_destination_fk FOREIGN KEY (destination_id) REFERENCES airports(id),
    CONSTRAINT flights_distinct_airports CHECK (origin_id <> destination_id),
    INDEX flights_departure_idx (departure_at)
) ENGINE=InnoDB;

INSERT INTO airports (code,name,city) VALUES
('CGK','Soekarno-Hatta','Tangerang'),
('DPS','I Gusti Ngurah Rai','Denpasar'),
('SUB','Juanda','Surabaya');

INSERT INTO flights(flight_number,origin_id,destination_id,departure_at) VALUES
('IF101',1,2,'2026-10-25 09:00:00'),
('IF102',2,1,'2026-10-25 13:00:00'),
('IF103',1,3,'2026-10-25 10:30:00');
