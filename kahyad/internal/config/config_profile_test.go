package config

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestProfileResolutionProdPathsByteIdentical is the W78-02 D1 guard: the
// generic KAHYA_ENV=<name> profile resolution must NOT change any production
// path. env="" and env="prod" both resolve to the EXACT current prod paths;
// a non-prod profile ("dev", "redteam") resolves to its own "-<name>"-
// suffixed tree, with the prod tree never mentioned. Every assertion is a
// byte-for-byte path comparison so a stray suffix/typo in the resolver
// surfaces here rather than as a silent data-directory drift.
func TestProfileResolutionProdPathsByteIdentical(t *testing.T) {
	clearEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)

	prodData := filepath.Join(home, "Library", "Application Support", "Kahya")
	prodKahya := filepath.Join(home, "Kahya")

	cases := []struct {
		name        string
		env         string
		wantDataDir string
		wantSocket  string
		wantDBPath  string
		wantMemDir  string
		wantKahya   string
		wantBackup  string
	}{
		{
			name:        "empty env is production",
			env:         "",
			wantDataDir: prodData,
			wantSocket:  filepath.Join(prodData, "kahyad.sock"),
			wantDBPath:  filepath.Join(prodData, "brain.db"),
			wantMemDir:  filepath.Join(prodKahya, "memory"),
			wantKahya:   prodKahya,
			wantBackup:  filepath.Join(prodKahya, "backups"),
		},
		{
			name:        "explicit prod is production",
			env:         "prod",
			wantDataDir: prodData,
			wantSocket:  filepath.Join(prodData, "kahyad.sock"),
			wantDBPath:  filepath.Join(prodData, "brain.db"),
			wantMemDir:  filepath.Join(prodKahya, "memory"),
			wantKahya:   prodKahya,
			wantBackup:  filepath.Join(prodKahya, "backups"),
		},
		{
			name:        "dev profile",
			env:         "dev",
			wantDataDir: filepath.Join(home, "Library", "Application Support", "Kahya-dev"),
			wantSocket:  filepath.Join(home, "Library", "Application Support", "Kahya-dev", "kahyad-dev.sock"),
			wantDBPath:  filepath.Join(home, "Library", "Application Support", "Kahya-dev", "brain.db"),
			wantMemDir:  filepath.Join(home, "Kahya-dev", "memory"),
			wantKahya:   filepath.Join(home, "Kahya-dev"),
			wantBackup:  filepath.Join(home, "Kahya-dev", "backups"),
		},
		{
			name:        "redteam profile (W78-05 reuse of the same generic resolution)",
			env:         "redteam",
			wantDataDir: filepath.Join(home, "Library", "Application Support", "Kahya-redteam"),
			wantSocket:  filepath.Join(home, "Library", "Application Support", "Kahya-redteam", "kahyad-redteam.sock"),
			wantDBPath:  filepath.Join(home, "Library", "Application Support", "Kahya-redteam", "brain.db"),
			wantMemDir:  filepath.Join(home, "Kahya-redteam", "memory"),
			wantKahya:   filepath.Join(home, "Kahya-redteam"),
			wantBackup:  filepath.Join(home, "Kahya-redteam", "backups"),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("KAHYA_ENV", tc.env)
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load(KAHYA_ENV=%q) error = %v", tc.env, err)
			}
			checks := []struct {
				field string
				got   string
				want  string
			}{
				{"DataDir", cfg.DataDir, tc.wantDataDir},
				{"Socket", cfg.Socket, tc.wantSocket},
				{"DBPath", cfg.DBPath, tc.wantDBPath},
				{"MemoryDir", cfg.MemoryDir, tc.wantMemDir},
				{"KahyaDir", cfg.KahyaDir, tc.wantKahya},
				{"BackupDir", cfg.BackupDir, tc.wantBackup},
			}
			for _, c := range checks {
				if c.got != c.want {
					t.Errorf("%s = %q, want %q", c.field, c.got, c.want)
				}
			}
			// A non-prod profile must never resolve ANY of its data paths
			// under the prod tree - the whole point of the isolation.
			if tc.env != "" && tc.env != EnvProd {
				for _, p := range []string{cfg.DataDir, cfg.DBPath, cfg.MemoryDir, cfg.KahyaDir} {
					if p == prodData || strings.HasPrefix(p, prodData+string(filepath.Separator)) ||
						p == prodKahya || strings.HasPrefix(p, prodKahya+string(filepath.Separator)) {
						t.Errorf("non-prod profile %q leaked a path into the prod tree: %q", tc.env, p)
					}
				}
			}
		})
	}
}

// TestProdDBPathMatchesProdLoad proves the exported ProdDBPath helper (used
// by the W78-02 red-team harness's fail-closed guard) returns exactly what a
// KAHYA_ENV=prod Load resolves DBPath to.
func TestProdDBPathMatchesProdLoad(t *testing.T) {
	clearEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("KAHYA_ENV", "prod")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got, err := ProdDBPath()
	if err != nil {
		t.Fatalf("ProdDBPath: %v", err)
	}
	if got != cfg.DBPath {
		t.Errorf("ProdDBPath() = %q, want %q (prod Load's DBPath)", got, cfg.DBPath)
	}
}

// TestValidateEnvAcceptsProfilesRejectsTraversal proves the generalized
// validateEnv accepts well-formed profile names and fails Load closed on any
// name that could traverse directories or otherwise smuggle a bad path
// component into "Kahya-<name>".
func TestValidateEnvAcceptsProfilesRejectsTraversal(t *testing.T) {
	for _, ok := range []string{"", "prod", "dev", "redteam", "restore", "scratch-1"} {
		if err := validateEnv(ok); err != nil {
			t.Errorf("validateEnv(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"../Kahya", "dev/../..", "prod ", "DEV", "a b", "dev\n", "..", "/etc", "de.v", "dev_x"} {
		if err := validateEnv(bad); err == nil {
			t.Errorf("validateEnv(%q) = nil, want a fail-closed error", bad)
		}
	}
}

// TestLoadRefusesNonProdProfilePointedAtProdDB proves the generalized
// refuseNonProdProfileOpeningProdDB guard fails Load closed when a non-prod
// profile is (via a stray KAHYA_DB_PATH override) aimed at the real prod
// brain.db - the HARD CONSTRAINT that no dev/red-team run can ever open prod.
func TestLoadRefusesNonProdProfilePointedAtProdDB(t *testing.T) {
	clearEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("KAHYA_ENV", "redteam")
	prodDB := filepath.Join(home, "Library", "Application Support", "Kahya", "brain.db")
	t.Setenv("KAHYA_DB_PATH", prodDB)

	if _, err := Load(); err == nil {
		t.Fatal("Load(KAHYA_ENV=redteam, KAHYA_DB_PATH=<prod brain.db>) = nil error, want a fail-closed refusal")
	} else if !strings.Contains(err.Error(), "production brain.db") {
		t.Errorf("error = %v, want it to mention refusing the production brain.db", err)
	}
}
