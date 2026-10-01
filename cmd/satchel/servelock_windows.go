package main

import "os"

// Windows 上 serve 直接拒绝（只保证客户端子命令），不需要数据目录的锁。
func acquireServeLock(string) (*os.File, error) { return nil, nil }

func execEnv(*os.File) []string { return os.Environ() }
