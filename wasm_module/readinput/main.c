// 登録済みデータ（指定順に /data/input0, /data/input1, ... として
// 読み取り専用でマウントされる）を順に連結して stdout に書き出すサンプル。
// POST /execute（JSON ボディの "data":["<id>",...]）の動作確認用
#include <stdio.h>

int main(void) {
    for (int i = 0;; i++) {
        char path[32];
        snprintf(path, sizeof path, "/data/input%d", i);
        FILE *f = fopen(path, "rb");
        if (!f) {
            if (i == 0) {
                perror("open /data/input0");
                return 1;
            }
            return 0;
        }
        char buf[4096];
        size_t n;
        while ((n = fread(buf, 1, sizeof buf, f)) > 0) {
            fwrite(buf, 1, n, stdout);
        }
        fclose(f);
    }
}
