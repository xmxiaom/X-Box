# -*- coding: utf-8 -*-
"""生成 X-Box 图标（纯 Python，PNG 手工编码，无第三方依赖）。
设计：蓝青渐变圆角方块 + 白色工具箱（拎手 + 盖缝 + 锁扣），
呼应「飞牛系统综合应用工具箱」定位，仿 fnOS 应用图标风格。
"""
import zlib, struct, math

SIZE = 256

def lerp(a, b, t):
    return tuple(int(a[i] + (b[i] - a[i]) * t) for i in range(3))

C1 = (0x16, 0x5D, 0xFF)  # 蓝
C2 = (0x14, 0xC9, 0xC9)  # 青

def in_rr(x, y, x0, y0, x1, y1, r):
    """任意位置圆角矩形命中测试（坐标 256 空间）。"""
    if x < x0 or y < y0 or x >= x1 or y >= y1:
        return False
    nx = min(x - x0, x1 - 1 - x)
    ny = min(y - y0, y1 - 1 - y)
    if nx >= r or ny >= r:
        return True
    dx, dy = r - nx, r - ny
    return dx * dx + dy * dy <= r * r

def draw_toolbox(gx, gy):
    """工具箱：白色箱体 + 顶部拎手（环形）+ 盖缝（透出渐变）+ 锁扣。
    返回白色 (255,255,255) 表示该像素为图形；返回 None 表示沿用渐变背景。
    """
    # 拎手：外环矩形 - 内孔 = 环形把手
    if in_rr(gx, gy, 104, 56, 152, 100, 14):
        if not in_rr(gx, gy, 116, 68, 140, 100, 10):
            return (255, 255, 255)
        return None
    # 箱体
    if in_rr(gx, gy, 56, 100, 200, 194, 20):
        # 锁扣（盖缝中央的小圆，盖住盖缝）
        if (gx - 128) ** 2 + (gy - 132) ** 2 <= 11 * 11:
            return (255, 255, 255)
        # 盖缝：透出渐变背景，形成箱体上下分界
        if 128 <= gy <= 136 and 60 <= gx <= 196:
            return None
        return (255, 255, 255)
    return None

def build_image(size):
    scale = SIZE / size
    rows = []
    for y in range(size):
        row = bytearray([0])  # filter type 0
        for x in range(size):
            # 以 256 坐标系计算再按比例采样（最近邻）
            sx, sy = int(x * scale), int(y * scale)
            m = SIZE - 1
            pad = int(14 * scale)
            r = int(52 * scale)
            inside = in_rr(sx, sy, pad, pad, SIZE - pad, SIZE - pad, r)
            if not inside:
                row.append(0); row.append(0); row.append(0); row.append(0)
                continue
            t = (sx + sy) / (2.0 * SIZE)
            c = lerp(C1, C2, t)
            alpha = 255
            lx, ly = sx - pad, sy - pad  # 图形坐标（0..228）
            gx, gy = int(lx / scale), int(ly / scale)
            tool = draw_toolbox(gx, gy)
            if tool is not None:
                c = tool
            row.extend([c[0], c[1], c[2], alpha])
        rows.append(bytes(row))
    return rows

def write_png(path, size):
    rows = build_image(size)
    raw = b"".join(rows)
    def chunk(tag, data):
        c = struct.pack(">I", len(data)) + tag + data
        c += struct.pack(">I", zlib.crc32(tag + data) & 0xFFFFFFFF)
        return c
    ihdr = struct.pack(">IIBBBBB", size, size, 8, 6, 0, 0, 0)
    png = b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", ihdr) + chunk(b"IDAT", zlib.compress(raw, 9)) + chunk(b"IEND", b"")
    with open(path, "wb") as f:
        f.write(png)
    print(f"OK {path} ({size}x{size}, {len(png)} bytes)")

if __name__ == "__main__":
    # 四处图标必须同源同尺寸（飞牛安装预览读 ICON.PNG，桌面读 ui/images/icon_256.png）：
    # ICON.PNG 历史上是 64px，安装向导预览放大显示会模糊，与桌面图标观感不一致。
    # 现四枚全部输出 256x256 高质量渲染，任何读取路径都是同一张清晰图。
    # 注意：Windows 文件系统大小写不敏感，ICON_256.PNG 与 icon_256.png 会互相覆盖，
    # 因此必须先生成 pkg 根两枚、再生成 ui/images 两枚，且目录不能混。
    write_png("pkg/ICON_256.PNG", 256)
    write_png("pkg/ICON.PNG", 256)
    write_png("pkg/app/ui/images/icon_256.png", 256)
    write_png("pkg/app/ui/images/icon_64.png", 256)
