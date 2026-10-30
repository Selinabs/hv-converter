from PIL import Image, ImageDraw
import math

S = 1024
img = Image.new("RGBA", (S, S), (0, 0, 0, 0))
d = ImageDraw.Draw(img)

# 圆角矩形渐变底
radius = int(S * 0.22)
grad_top = (77, 107, 254)     # #4D6BFE
grad_bottom = (24, 30, 66)    # #181E42

mask = Image.new("L", (S, S), 0)
md = ImageDraw.Draw(mask)
md.rounded_rectangle([0, 0, S - 1, S - 1], radius=radius, fill=255)

grad = Image.new("RGBA", (S, S))
gd = ImageDraw.Draw(grad)
for y in range(S):
    t = y / (S - 1)
    # 加一点曲线让过渡更自然
    tt = t ** 0.85
    r = int(grad_top[0] * (1 - tt) + grad_bottom[0] * tt)
    g = int(grad_top[1] * (1 - tt) + grad_bottom[1] * tt)
    b = int(grad_top[2] * (1 - tt) + grad_bottom[2] * tt)
    gd.line([(0, y), (S, y)], fill=(r, g, b, 255))

img.paste(grad, (0, 0), mask)
d = ImageDraw.Draw(img)

# 内侧高光边
d.rounded_rectangle([S * 0.035, S * 0.035, S * 0.965, S * 0.965],
                    radius=int(radius * 0.92), outline=(255, 255, 255, 40), width=int(S * 0.012))

# 播放三角（白色，圆角效果用多边形近似）
cx, cy = S * 0.52, S * 0.5
w, h = S * 0.30, S * 0.36
pts = [
    (cx - w * 0.42, cy - h / 2),
    (cx - w * 0.42, cy + h / 2),
    (cx + w * 0.58, cy),
]
d.polygon(pts, fill=(255, 255, 255, 245))

img = img.resize((512, 512), Image.LANCZOS)
img.save("app.png")

sizes = [(16, 16), (24, 24), (32, 32), (48, 48), (64, 64), (128, 128), (256, 256)]
img.save("app.ico", sizes=sizes)
print("icon written: app.ico")
