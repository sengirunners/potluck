package runtarget

import "syscall"

func ctimeOf(st *syscall.Stat_t) (int64, bool) {
	return st.Ctimespec.Sec*1e9 + st.Ctimespec.Nsec, true
}
