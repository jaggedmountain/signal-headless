# SPDX-FileCopyrightText: 2026 Jeff Mattson
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# CMake toolchain file for BoringSSL (built by libsignal's boring-sys) on
# Windows with MSYS2 MINGW64 gcc. Set through
# CMAKE_TOOLCHAIN_FILE_x86_64_pc_windows_gnu; see .github/workflows.
#
# Native Windows builds would use BoringSSL's NASM assembly, which lacks the
# GNU-assembler-only ADX routines its headers call under gcc
# (fiat_p256_adx_*), so the final link fails. Portable C instead.
set(CMAKE_C_COMPILER gcc)
set(CMAKE_CXX_COMPILER g++)
set(OPENSSL_NO_ASM ON CACHE BOOL "" FORCE)
