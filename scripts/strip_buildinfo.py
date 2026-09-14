#!/usr/bin/env python3
"""
Go 二进制元数据剥离与指纹消除工具 (Go Binary Strip & Fingerprint Scrambler)
功能：
1. 抹除 `\xff Go buildinf:` 模块信息魔数，使 `go version -m` 无法读取依赖与 VCS 信息
2. 注入哈希扰动数据，彻底打乱二进制的 SHA256 与模糊哈希指纹，脱离已知官方哈希特征库
"""

import sys
import os
import secrets

GO_BUILDINFO_MAGIC = b"\xff Go buildinf:"

def strip_binary(file_path: str) -> bool:
    if not os.path.exists(file_path):
        print(f"[-] 文件不存在: {file_path}", file=sys.stderr)
        return False

    with open(file_path, "rb") as f:
        data = bytearray(f.read())

    magic_count = 0
    idx = 0
    while True:
        idx = data.find(GO_BUILDINFO_MAGIC, idx)
        if idx == -1:
            break
        # 将魔数替换为无害的随机填充字节，使 go version -m 完全无法定位 buildinfo
        scramble = secrets.token_bytes(len(GO_BUILDINFO_MAGIC))
        data[idx:idx + len(GO_BUILDINFO_MAGIC)] = scramble
        magic_count += 1
        idx += len(GO_BUILDINFO_MAGIC)

    # 注入安全哈希扰动块（在文件末尾追加伪随机熵数据，不影响 ELF/PE 代码段执行）
    # 这会使文件的哈希（SHA256、MD5、ssdeep）产生彻底突变，脱离任何官方特征库匹配
    random_padding = secrets.token_bytes(64)
    data.extend(random_padding)

    with open(file_path, "wb") as f:
        f.write(data)

    print(f"[+] 成功处理: {file_path}")
    print(f"    - 抹除 buildinfo 锚点: {magic_count} 处")
    print(f"    - 注入随机指纹扰动: 64 字节")
    return True

if __name__ == "__main__":
    if len(sys.argv) < 2:
        print(f"用法: {sys.argv[0]} <二进制文件路径> [更多文件...]")
        sys.exit(1)

    for target in sys.argv[1:]:
        strip_binary(target)
