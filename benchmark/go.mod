module github.com/nelthaarion/breezeorm-benchmark

go 1.23.0

replace github.com/nelthaarion/breezeorm => ../

require (
	github.com/jackc/pgx/v5 v5.5.5
	github.com/jmoiron/sqlx v1.4.0
	github.com/nelthaarion/breezeorm v0.0.0-00010101000000-000000000000
	github.com/uptrace/bun v1.2.9
	github.com/uptrace/bun/dialect/pgdialect v1.2.9
	gorm.io/driver/postgres v1.5.7
	gorm.io/gorm v1.25.12
)

require (
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20221227161230-091c0ba34f0a // indirect
	github.com/jackc/puddle/v2 v2.2.1 // indirect
	github.com/jinzhu/inflection v1.0.0 // indirect
	github.com/jinzhu/now v1.1.5 // indirect
	github.com/mattn/go-sqlite3 v1.14.47 // indirect
	github.com/puzpuzpuz/xsync/v3 v3.5.0 // indirect
	github.com/tmthrgd/go-hex v0.0.0-20190904060850-447a3041c3bc // indirect
	github.com/vmihailenco/msgpack/v5 v5.4.1 // indirect
	github.com/vmihailenco/tagparser/v2 v2.0.0 // indirect
	golang.org/x/crypto v0.17.0 // indirect
	golang.org/x/sync v0.10.0 // indirect
	golang.org/x/sys v0.29.0 // indirect
	golang.org/x/text v0.14.0 // indirect
)

replace filippo.io/edwards25519 => github.com/FiloSottile/edwards25519 v1.1.0

replace golang.org/x/sys => github.com/golang/sys v0.29.0

replace golang.org/x/text => github.com/golang/text v0.21.0

replace golang.org/x/exp => github.com/golang/exp v0.0.0-20260611194520-c48552f49976

replace gorm.io/gorm => github.com/go-gorm/gorm v1.25.12

replace gorm.io/driver/sqlite => github.com/go-gorm/sqlite v1.5.7

replace gorm.io/driver/postgres => github.com/go-gorm/postgres v1.5.7
