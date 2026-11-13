package textclean

import (
	_ "embed"
	"strings"
)

// Unicode 17.0 emoji-zwj-sequences.txt; see UNICODE-LICENSE.
//
//go:embed emoji-zwj.txt
var emojiSequences string

type emojiNode struct {
	next     map[rune]*emojiNode
	terminal bool
}

var emojiTree = makeEmojiTree()

func makeEmojiTree() *emojiNode {
	root := &emojiNode{}
	var insert func(*emojiNode, []rune)
	insert = func(n *emojiNode, rs []rune) {
		if len(rs) == 0 {
			n.terminal = true
			return
		}
		if rs[0] == 0xfe0f {
			insert(n, rs[1:])
		}
		if n.next == nil {
			n.next = make(map[rune]*emojiNode)
		}
		if n.next[rs[0]] == nil {
			n.next[rs[0]] = &emojiNode{}
		}
		insert(n.next[rs[0]], rs[1:])
	}
	for _, seq := range strings.Split(strings.TrimSpace(emojiSequences), "\n") {
		insert(root, []rune(seq))
	}

	return root
}

// emojiJoiners returns positions of joiners in complete recognized sequences.
func emojiJoiners(rs []rune) map[int]bool {
	var valid map[int]bool
	for i := 0; i < len(rs); i++ {
		n := emojiTree
		for j := i; j < len(rs); j++ {
			n = n.next[rs[j]]
			if n == nil {
				break
			}
			if n.terminal {
				for k := i; k <= j; k++ {
					if rs[k] == 0x200d {
						if valid == nil {
							valid = make(map[int]bool)
						}
						valid[k] = true
					}
				}
			}
		}
	}
	return valid
}
