// 登録済みデータ（/data/input に読み取り専用でマウントされる）を
// そのまま stdout に書き出すサンプル。
// POST /data/{id}/execute の動作確認用
#include <stdio.h>

int main(void) {
    FILE *f = fopen("/data/input", "rb");
    if (!f) {
        perror("open /data/input");
        return 1;
    }
    char buf[4096];
    size_t n;
    while ((n = fread(buf, 1, sizeof buf, f)) > 0) {
        fwrite(buf, 1, n, stdout);
    }
    fclose(f);
    return 0;
}
