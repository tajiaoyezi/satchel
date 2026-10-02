// Package overview 是业务层的开局总览（master-overview）：overview 一条命令返回服务器、用户、告警、待办四个分区，
// 每个分区是 items（至多 10 条）加 total。分区随对应功能出现——服务器 M2、用户 M3、告警与待办 M4，在那之前是空数组加 0；
// 哪一站交付哪个分区，就由那一站给这里加它需要的读取函数，在装配根注入（业务模块之间不互相引用）。
package overview

import (
	"context"

	"github.com/satchel/satchel/internal/command"
)

// Partition 是一个分区：items 是按交付这个分区的那一站定的排序取的前几条，total 是这个调用者看得到的总条数。
type Partition struct {
	Items []any `json:"items"`
	Total int   `json:"total"`
}

// Result 是 overview 的输出：四个分区一个不少。
type Result struct {
	Servers Partition `json:"servers"`
	Users   Partition `json:"users"`
	Alerts  Partition `json:"alerts"`
	Tasks   Partition `json:"tasks"`
}

// Service 是开局总览。M1 的四个分区都还没有数据来源。
type Service struct{}

// New 建开局总览服务。
func New() *Service { return &Service{} }

// Bindings 是 overview 的处理函数。
func (s *Service) Bindings() command.Bindings {
	return command.Bindings{"overview": s.overview}
}

// empty 是还没交付的分区：items 是空数组（JSON 里是 []，不是 null），total 为 0。
func empty() Partition { return Partition{Items: []any{}} }

func (s *Service) overview(context.Context, *command.Invocation) (any, error) {
	return Result{Servers: empty(), Users: empty(), Alerts: empty(), Tasks: empty()}, nil
}
