import hashlib
import json
import os
import struct
import subprocess
import sys
import time
import zlib
import zipfile

ROOT = os.path.dirname(os.path.abspath(__file__))
MANIFEST = os.path.join(ROOT, "plugin.json")
DIST = os.path.join(ROOT, "dist")
BIN = os.path.join(DIST, "tgdrama-android")
OUT = os.environ.get("PACK_OUT") or os.path.join(ROOT, "tgdrama-plugin.zip")

def build(goos, goarch, out):
    env = dict(os.environ, CGO_ENABLED="0", GOOS=goos, GOARCH=goarch)
    print("[build] %s/%s -> %s" % (goos, goarch, out))
    subprocess.run(

        ["go", "build", "-tags", "nodynamic", "-trimpath",
         "-ldflags", "-s -w -buildid=", "-o", out, "./cmd/tgdrama"],
        cwd=ROOT, env=env, check=True,
    )

def check_elf(path):
    with open(path, "rb") as f:
        magic = f.read(4)
    if magic != b"\x7fELF":
        sys.exit("二进制不是 ELF（魔数 %r），检查 GOOS/GOARCH 环境变量是否生效" % magic)
    print("[check] ELF 魔数 OK")

def export_rule():
    """导出侧载规则页：与内置首页同一份，只多一行宿主播放标记（内置页不推播放器）。"""
    src = os.path.join(ROOT, "internal", "web", "static", "index.html")
    dst = os.path.join(ROOT, "tg-drama-rule.html")
    html = open(src, "rb").read().decode("utf-8")
    marker = "<script>window.__TG_RULE_HOST__ = true;</script>"
    if marker not in html:
        if "</head>" in html:
            html = html.replace("</head>", marker + "</head>", 1)
        else:
            html = marker + html
    data = html.encode("utf-8")
    if os.path.exists(dst) and open(dst, "rb").read() == data:
        print("[rule] tg-drama-rule.html 已是最新 (%d bytes)" % len(data))
        return
    open(dst, "wb").write(data)
    print("[rule] 已导出 tg-drama-rule.html (%d bytes)" % len(data))

def deflate(data):
    """DEFLATE 压缩：能用 zopfli 就用（比 zlib -9 再小约 4%，代价是慢几十秒），
    否则退回 zlib level 9。返回裸 deflate 流（zip 的 method 8 要求，不带 zlib 头尾）。"""
    try:
        import zopfli.zlib as zz

        stream = zz.compress(data)

        if len(stream) > 6 and (stream[0] & 0x0F) == 8 and (stream[1] & 0x20) == 0:
            return stream[2:-4]
    except Exception as exc:
        print("[zip] zopfli 不可用（%s），退回 zlib -9" % exc)
    c = zlib.compressobj(9, zlib.DEFLATED, -15, 9)
    return c.compress(data) + c.flush()

def write_zip(entries, out):
    """手写 ZIP：python 的 zipfile 不支持外挂压缩器，而 zopfli 能再省几 MB。
    entries = [(源文件, zip 内路径, unix 权限)]，顺序即写入顺序。"""
    stamp = time.localtime(max(os.path.getmtime(src) for src, _, _ in entries))
    dos_time = (stamp.tm_hour << 11) | (stamp.tm_min << 5) | (stamp.tm_sec // 2)
    dos_date = ((stamp.tm_year - 1980) << 9) | (stamp.tm_mon << 5) | stamp.tm_mday
    body = bytearray()
    central = bytearray()
    for src, arc, mode in entries:
        data = open(src, "rb").read()
        comp = deflate(data)
        name = arc.encode("utf-8")
        crc = zlib.crc32(data) & 0xFFFFFFFF
        offset = len(body)
        body += struct.pack(
            "<IHHHHHIIIHH", 0x04034B50, 20, 0, 8, dos_time, dos_date,
            crc, len(comp), len(data), len(name), 0,
        )
        body += name + comp
        central += struct.pack(
            "<IHHHHHHIIIHHHHHII", 0x02014B50, 0x031E, 20, 0, 8, dos_time, dos_date,
            crc, len(comp), len(data), len(name), 0, 0, 0, 0, mode << 16, offset,
        )
        central += name
        print("  + %-18s %10d -> %9d bytes (%.1f%%)  mode=%o"
              % (arc, len(data), len(comp), 100.0 * len(comp) / max(len(data), 1), mode))
    eocd = struct.pack(
        "<IHHHHIIH", 0x06054B50, 0, 0, len(entries), len(entries), len(central), len(body), 0,
    )
    with open(out, "wb") as f:
        f.write(bytes(body) + bytes(central) + eocd)

def md5(path):
    h = hashlib.md5()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()

def main():
    os.makedirs(DIST, exist_ok=True)
    export_rule()
    build("android", "arm64", BIN)
    check_elf(BIN)
    if "--windows" in sys.argv:
        build("windows", "amd64", os.path.join(DIST, "tgdrama-windows.exe"))

    man = json.load(open(MANIFEST, encoding="utf-8"))
    assert man["binaries"]["android"] == "tgdrama-android", man["binaries"]

    if os.path.exists(OUT):
        os.remove(OUT)
    write_zip(
        [(MANIFEST, "plugin.json", 0o100644), (BIN, "tgdrama-android", 0o100755)],
        OUT,
    )

    with zipfile.ZipFile(OUT) as z:
        names = z.namelist()
        assert names == ["plugin.json", "tgdrama-android"], names
        assert z.testzip() is None
        inner = {i.filename: i for i in z.infolist()}
        assert inner["tgdrama-android"].external_attr >> 16 & 0o777 == 0o755
        assert z.read("tgdrama-android")[:4] == b"\x7fELF"
        pj = json.loads(z.read("plugin.json"))
        assert pj["name"] == "tgdrama" and pj["runtime"] == "binary"

    size = os.path.getsize(OUT)
    print("[pack] %s: %d bytes (%.2f MB), md5=%s" % (OUT, size, size / 1048576.0, md5(OUT)))
    print("[pack] 结构断言通过")

if __name__ == "__main__":
    main()
