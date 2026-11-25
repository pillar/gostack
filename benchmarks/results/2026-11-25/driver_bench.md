| benchmark | runs | ns/op min | ns/op median | ns/op max | spread | B/op | allocs/op |
|---|---|---|---|---|---|---|---|
| WriteFULL/modernc | 5 | 98732 | 115094 | 135646 | 32.1% | 807 | 22 |
| WriteFULL/modernc-4 | 5 | 79587 | 104421 | 118178 | 37.0% | 807 | 22 |
| WriteFULL/mattn | 5 | 78830 | 99246 | 108329 | 29.7% | 943 | 28 |
| WriteFULL/mattn-4 | 5 | 76855 | 88835 | 131868 | 61.9% | 943 | 28 |
| WriteFULL/modernc/fullfsync | 5 | 3510365 | 4241329 | 4458563 | 22.4% | 776 | 20 |
| WriteFULL/modernc/fullfsync-4 | 5 | 3323316 | 3360022 | 3880323 | 16.6% | 787 | 20 |
| WriteFULL/mattn/fullfsync | 5 | 3330155 | 3450577 | 3504906 | 5.1% | 927 | 26 |
| WriteFULL/mattn/fullfsync-4 | 5 | 3251744 | 4471621 | 5061019 | 40.5% | 925 | 26 |
| BatchInsert1000/modernc | 5 | 5498897 | 5724057 | 6011413 | 9.0% | 328706 | 13010 |
| BatchInsert1000/modernc-4 | 5 | 5653423 | 10094008 | 12664325 | 69.5% | 328737 | 13007 |
| BatchInsert1000/mattn | 5 | 5796020 | 6578676 | 9147042 | 50.9% | 288879 | 11017 |
| BatchInsert1000/mattn-4 | 5 | 4774608 | 5976162 | 8324288 | 59.4% | 288949 | 11018 |
| PointRead4/modernc | 5 | 5522 | 5914 | 6228 | 11.9% | 2784 | 25 |
| PointRead4/modernc-4 | 5 | 4426 | 4472 | 4506 | 1.8% | 2784 | 25 |
| PointRead4/mattn | 5 | 3550 | 3730 | 3852 | 8.1% | 2623 | 17 |
| PointRead4/mattn-4 | 5 | 3489 | 3674 | 5396 | 51.9% | 2624 | 17 |
