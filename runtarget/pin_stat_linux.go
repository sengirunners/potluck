package runtarget

import "syscall"

func ctimeOf(st *syscall.Stat_t) (int64, bool) {
	return int64(st.Ctim.Sec)*1e9 + int64(st.Ctim.Nsec), true
}
