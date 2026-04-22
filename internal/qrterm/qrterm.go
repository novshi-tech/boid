package qrterm

import (
	"strings"

	"rsc.io/qr"
)

const margin = 1

// Encode は text を QR コード化してターミナル文字列を返す。
// ascii=true の場合は "##" / "  " でフォールバック描画する。
func Encode(text string, ascii bool) (string, error) {
	code, err := qr.Encode(text, qr.M)
	if err != nil {
		return "", err
	}

	size := code.Size
	W := size + 2*margin
	H := size + 2*margin

	black := func(col, row int) bool {
		qx, qy := col-margin, row-margin
		return qx >= 0 && qx < size && qy >= 0 && qy < size && code.Black(qx, qy)
	}

	var sb strings.Builder

	if ascii {
		for row := 0; row < H; row++ {
			for col := 0; col < W; col++ {
				if black(col, row) {
					sb.WriteString("##")
				} else {
					sb.WriteString("  ")
				}
			}
			sb.WriteByte('\n')
		}
	} else {
		for charRow := 0; charRow*2 < H; charRow++ {
			row0 := charRow * 2
			row1 := charRow*2 + 1
			for col := 0; col < W; col++ {
				top := black(col, row0)
				bot := row1 < H && black(col, row1)
				switch {
				case top && bot:
					sb.WriteRune('█')
				case top && !bot:
					sb.WriteRune('▀')
				case !top && bot:
					sb.WriteRune('▄')
				default:
					sb.WriteByte(' ')
				}
			}
			sb.WriteByte('\n')
		}
	}

	return sb.String(), nil
}
