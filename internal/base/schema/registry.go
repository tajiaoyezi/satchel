package schema

// Default 返回 Satchel 的表注册表：12 张 agent-native 表、mmwx 的核心 kind 五簇与照抄七簇，共 87 张表。
// 注册表是静态数据，校验不过说明代码写错了，直接 panic；tables_test.go 保证它过。
func Default() *Registry {
	r := New()
	for _, group := range [][]Table{
		agentNativeTables(), userTables(), packageTables(), serverTables(), nodeTables(),
		subscriptionTables(), certificateTables(), trafficTables(), opsTables(), federationTables(), telegramTables(),
	} {
		for _, t := range group {
			r.Add(t)
		}
	}
	if err := r.Validate(); err != nil {
		panic(err)
	}
	return r
}
