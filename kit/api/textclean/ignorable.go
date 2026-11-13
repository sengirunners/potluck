// Default_Ignorable_Code_Point from Unicode 17.0 DerivedCoreProperties.txt.
// Unicode data terms: https://www.unicode.org/license.txt
package textclean

import "unicode"

var defaultIgnorable = &unicode.RangeTable{
	R16: []unicode.Range16{
		{Lo: 0xad, Hi: 0xad, Stride: 1},
		{Lo: 0x34f, Hi: 0x34f, Stride: 1},
		{Lo: 0x61c, Hi: 0x61c, Stride: 1},
		{Lo: 0x115f, Hi: 0x1160, Stride: 1},
		{Lo: 0x17b4, Hi: 0x17b5, Stride: 1},
		{Lo: 0x180b, Hi: 0x180d, Stride: 1},
		{Lo: 0x180e, Hi: 0x180e, Stride: 1},
		{Lo: 0x180f, Hi: 0x180f, Stride: 1},
		{Lo: 0x200b, Hi: 0x200f, Stride: 1},
		{Lo: 0x202a, Hi: 0x202e, Stride: 1},
		{Lo: 0x2060, Hi: 0x2064, Stride: 1},
		{Lo: 0x2065, Hi: 0x2065, Stride: 1},
		{Lo: 0x2066, Hi: 0x206f, Stride: 1},
		{Lo: 0x3164, Hi: 0x3164, Stride: 1},
		{Lo: 0xfe00, Hi: 0xfe0f, Stride: 1},
		{Lo: 0xfeff, Hi: 0xfeff, Stride: 1},
		{Lo: 0xffa0, Hi: 0xffa0, Stride: 1},
		{Lo: 0xfff0, Hi: 0xfff8, Stride: 1},
	},
	R32: []unicode.Range32{
		{Lo: 0x1bca0, Hi: 0x1bca3, Stride: 1},
		{Lo: 0x1d173, Hi: 0x1d17a, Stride: 1},
		{Lo: 0xe0000, Hi: 0xe0000, Stride: 1},
		{Lo: 0xe0001, Hi: 0xe0001, Stride: 1},
		{Lo: 0xe0002, Hi: 0xe001f, Stride: 1},
		{Lo: 0xe0020, Hi: 0xe007f, Stride: 1},
		{Lo: 0xe0080, Hi: 0xe00ff, Stride: 1},
		{Lo: 0xe0100, Hi: 0xe01ef, Stride: 1},
		{Lo: 0xe01f0, Hi: 0xe0fff, Stride: 1},
	},
}
