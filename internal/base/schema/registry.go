package schema

// Default 返回 Satchel 的表注册表：12 张 agent-native 表加 mmwx 的核心 kind 五簇；照抄簇随最后一个 change 录入。
// 注册表是静态数据，校验不过说明代码写错了，直接 panic；tables_test.go 保证它过。
func Default() *Registry {
	r := New()
	for _, group := range [][]Table{agentNativeTables(), userTables(), packageTables(), serverTables(), nodeTables()} {
		for _, t := range group {
			r.Add(t)
		}
	}
	if err := r.Validate(); err != nil {
		panic(err)
	}
	return r
}
