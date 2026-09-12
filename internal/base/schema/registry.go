package schema

// Default 返回 Satchel 的表注册表。现在只有 12 张 agent-native 表，mmwx 的表随后两个 change 录入。
// 注册表是静态数据，校验不过说明代码写错了，直接 panic；tables_test.go 保证它过。
func Default() *Registry {
	r := New()
	for _, t := range agentNativeTables() {
		r.Add(t)
	}
	if err := r.Validate(); err != nil {
		panic(err)
	}
	return r
}
