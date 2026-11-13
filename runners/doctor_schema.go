package runners

import "github.com/sengirunners/potluck/kit/store"

var doctorMigrations = []store.Migration{
	{Kind: store.KindRegistry, Name: "runners/0009_doctor", Script: `CREATE TABLE runners_doctor (
 repo TEXT NOT NULL,
 check_number INTEGER NOT NULL,
 status TEXT NOT NULL,
 notified INTEGER NOT NULL,
 message TEXT NOT NULL,
 fix TEXT NOT NULL,
 checked INTEGER NOT NULL,
 installation INTEGER NOT NULL,
 PRIMARY KEY(repo,check_number)
 );`},
}
