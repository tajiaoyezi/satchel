// Package scheduler 是投影层的内置维护任务：定时以系统身份调 service，是第四个写者（master-scheduler）。
// 框架只管按时运行与记录（task_runs 经 service/schedule 写）；任务直接调 service、不经命令执行链，所以不写审计。
// 改配置的任务（M2 起）必须走带审计的写路径，不能只靠这里的运行记录。
package scheduler
