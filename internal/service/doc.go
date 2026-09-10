// Package service 是业务层。每个功能域一个子包（users、packages、servers、inbounds、plan……），
// 与 core 的子包一一对应；跨模块协作只在这一层，做法是持有别的模块的 core。
// 子包随功能代码建，本包自身没有代码。
package service
