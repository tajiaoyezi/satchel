package schema

// 生成物在仓库里的相对路径。
const (
	SQLiteMigrationPath   = "internal/base/db/migrations/sqlite/0001_init.tx.up.sql"
	PostgresMigrationPath = "internal/base/db/migrations/postgres/0001_init.tx.up.sql"
	ModelPath             = "internal/base/model/zz_generated.go"
	KindsPath             = "pkg/api/v1/zz_generated_kinds.go"
)

// Outputs 返回全部生成物：仓库相对路径 → 内容。cmd/gen 写文件，测试用它比对仓库里提交的版本。
func Outputs(r *Registry) (map[string][]byte, error) {
	models, err := GenerateModels(r)
	if err != nil {
		return nil, err
	}
	kinds, err := GenerateKinds(r)
	if err != nil {
		return nil, err
	}
	return map[string][]byte{
		SQLiteMigrationPath:   []byte(DDL(r, SQLite)),
		PostgresMigrationPath: []byte(DDL(r, Postgres)),
		ModelPath:             models,
		KindsPath:             kinds,
	}, nil
}
