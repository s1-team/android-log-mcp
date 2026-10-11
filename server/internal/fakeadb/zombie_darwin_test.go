package fakeadb

func zombie(pid int) bool {
	k, ok := kinfo(pid)
	return ok && k.Proc.P_stat == sZomb
}
